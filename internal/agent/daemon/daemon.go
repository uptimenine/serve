package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/uptimenine/serve/internal/agent/cutover"
	"github.com/uptimenine/serve/internal/agent/events"
	"github.com/uptimenine/serve/internal/agent/healing"
	"github.com/uptimenine/serve/internal/agent/health"
	"github.com/uptimenine/serve/internal/agent/proxy"
	"github.com/uptimenine/serve/internal/agent/proxy/kamalproxy"
	"github.com/uptimenine/serve/internal/agent/reconciler"
	"github.com/uptimenine/serve/internal/agent/secrets"
	"github.com/uptimenine/serve/internal/agent/secrets/sops"
	agentstate "github.com/uptimenine/serve/internal/agent/state"
	"github.com/uptimenine/serve/internal/planner"
	"github.com/uptimenine/serve/internal/runtime"
)

const (
	DefaultSocketPath        = "/run/serve/agent.sock"
	DefaultStateDir          = "/var/lib/serve/state"
	DefaultReconcileInterval = 10 * time.Second
	defaultEnvFileDir        = "/run/serve/env"
)

type Config struct {
	Runtime           runtime.Runtime
	StateDir          string
	SocketPath        string
	ReconcileInterval time.Duration

	// Optional overrides; production defaults are used when nil/empty.
	HealthChecker health.Checker
	ProxyManager  proxy.Manager
	SecretStore   secrets.Store
	EnvFileDir    string
	EventSink     healing.EventSink
	ErrorLog      io.Writer
}

// Daemon is the long-running host agent: it applies desired state through
// the cutover engine, heals containers from runtime events, reconciles
// periodically, and serves the local API on a Unix socket.
type Daemon struct {
	runtime    runtime.Runtime
	store      *agentstate.Store
	engine     *cutover.Engine
	supervisor *healing.Supervisor
	socketPath string
	interval   time.Duration
	errorLog   io.Writer

	mu             sync.RWMutex
	desired        map[string]planner.DesiredState
	operationLocks map[string]*sync.Mutex
	reconcileMu    sync.Mutex
	background     sync.WaitGroup
}

func New(cfg Config) *Daemon {
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = DefaultSocketPath
	}
	if cfg.ReconcileInterval <= 0 {
		cfg.ReconcileInterval = DefaultReconcileInterval
	}
	if cfg.HealthChecker == nil {
		cfg.HealthChecker = health.NewHTTPChecker(nil)
	}
	if cfg.ProxyManager == nil {
		cfg.ProxyManager = kamalproxy.New(cfg.Runtime, kamalproxy.Options{Network: "serve"})
	}
	if cfg.SecretStore == nil {
		cfg.SecretStore = sops.NewDefaultStore()
	}
	if cfg.EnvFileDir == "" {
		cfg.EnvFileDir = defaultEnvFileDir
	}
	if cfg.ErrorLog == nil {
		cfg.ErrorLog = os.Stderr
	}
	if cfg.EventSink == nil {
		cfg.EventSink = events.NewJSONSink(os.Stdout)
	}

	store := agentstate.NewStore(cfg.StateDir)
	starter := reconciler.NewWithSecrets(cfg.Runtime, cfg.SecretStore, secrets.NewEnvFileWriter(cfg.EnvFileDir))
	engine := cutover.New(cutover.Deps{
		Runtime:  cfg.Runtime,
		Starter:  starter,
		Health:   cfg.HealthChecker,
		Proxy:    cfg.ProxyManager,
		LastGood: store,
	})
	supervisor := healing.NewSupervisor(cfg.Runtime,
		healing.WithReconciler(starter),
		healing.WithProxyManager(cfg.ProxyManager),
		healing.WithEventSink(cfg.EventSink),
	)

	return &Daemon{
		runtime:        cfg.Runtime,
		store:          store,
		engine:         engine,
		supervisor:     supervisor,
		socketPath:     cfg.SocketPath,
		interval:       cfg.ReconcileInterval,
		errorLog:       cfg.ErrorLog,
		desired:        map[string]planner.DesiredState{},
		operationLocks: map[string]*sync.Mutex{},
	}
}

func (d *Daemon) Run(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(d.socketPath), 0o755); err != nil {
		return fmt.Errorf("create socket directory: %w", err)
	}
	_ = os.Remove(d.socketPath)
	listener, err := net.Listen("unix", d.socketPath)
	if err != nil {
		return fmt.Errorf("listen on agent socket: %w", err)
	}
	defer os.Remove(d.socketPath)
	// The socket is root-equivalent: it applies desired state and drives
	// Docker. Restrict it to the owner and group.
	if err := os.Chmod(d.socketPath, 0o660); err != nil {
		listener.Close()
		return fmt.Errorf("restrict agent socket permissions: %w", err)
	}

	events, err := d.runtime.Events(ctx)
	if err != nil {
		listener.Close()
		return fmt.Errorf("subscribe to runtime events: %w", err)
	}

	server := &http.Server{Handler: d.handler()}
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Serve(listener) }()
	defer server.Close()

	d.startBackgroundReconcile(ctx, "initial")
	defer d.background.Wait()

	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-serverErr:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("agent API server: %w", err)
			}
			return nil
		case event, ok := <-events:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("runtime event stream closed")
			}
			d.handleEvent(ctx, event)
		case <-ticker.C:
			d.startBackgroundReconcile(ctx, "periodic")
		}
	}
}

func (d *Daemon) startBackgroundReconcile(ctx context.Context, source string) {
	if !d.reconcileMu.TryLock() {
		return
	}
	d.background.Add(1)
	go func() {
		defer d.background.Done()
		defer d.reconcileMu.Unlock()
		if err := d.reconcileAll(ctx); err != nil && ctx.Err() == nil {
			d.logError("%s reconcile: %v", source, err)
		}
	}()
}

// reconcileAll reloads every persisted desired state and applies it. Desired
// state files written directly to disk (e.g. by `serve agent apply` over
// SSH) are picked up here.
func (d *Daemon) reconcileAll(ctx context.Context) error {
	states, err := d.store.ListDesired()
	if err != nil {
		return fmt.Errorf("list desired states: %w", err)
	}

	var errs []error
	for _, desired := range states {
		if err := d.applySerialized(ctx, desired); err != nil {
			errs = append(errs, fmt.Errorf("reconcile %s %s: %w", desired.Service, desired.Destination, err))
			continue
		}
		d.setDesired(desired)
	}
	return errors.Join(errs...)
}

func (d *Daemon) applySerialized(ctx context.Context, desired planner.DesiredState) error {
	lock := d.operationLock(stateKey(desired.Service, desired.Destination))
	lock.Lock()
	defer lock.Unlock()
	return d.apply(ctx, desired)
}

// apply runs one desired state through the cutover engine and records the
// resulting actual state. The caller must hold the state's operation lock.
func (d *Daemon) apply(ctx context.Context, desired planner.DesiredState) error {
	if err := agentstate.ValidateIdentity(desired.Service, desired.Destination); err != nil {
		return err
	}
	if err := planner.ValidateDesired(desired); err != nil {
		return err
	}
	if err := d.engine.Apply(ctx, desired); err != nil {
		return err
	}
	actual, err := d.actualState(ctx, desired.Service, desired.Destination)
	if err != nil {
		return err
	}
	return d.store.SaveActual(actual)
}

func (d *Daemon) handleEvent(ctx context.Context, event runtime.RuntimeEvent) {
	if event.Labels["serve.managed"] != "true" {
		return
	}

	key := stateKey(event.Labels["serve.service"], event.Labels["serve.destination"])
	lock := d.operationLock(key)
	lock.Lock()
	defer lock.Unlock()

	desired, ok := d.getDesired(key)
	if !ok {
		return
	}
	result, err := d.supervisor.HandleRuntimeEvent(ctx, event, desired)
	if err != nil {
		d.logError("heal %s: %v", event.Name, err)
		return
	}
	if result.Restarted {
		// Re-apply so a healed web container is routed again once healthy.
		if err := d.apply(ctx, desired); err != nil {
			d.logError("post-heal apply %s %s: %v", desired.Service, desired.Destination, err)
		}
	}
}

func (d *Daemon) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/desired-state", d.handlePutDesiredState)
	mux.HandleFunc("GET /v1/status", d.handleStatus)
	mux.HandleFunc("POST /v1/reconcile", d.handleReconcile)
	mux.HandleFunc("GET /v1/logs", d.handleLogs)
	mux.HandleFunc("GET /v1/events", d.handleEvents)
	return mux
}

func (d *Daemon) handleLogs(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("container")
	if name == "" {
		http.Error(w, "container query parameter is required", http.StatusBadRequest)
		return
	}

	containers, err := d.runtime.ListContainers(r.Context(), runtime.ContainerFilters{Labels: map[string]string{"serve.managed": "true"}})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var id runtime.ContainerID
	for _, container := range containers {
		if container.Name == name {
			id = container.ID
			break
		}
	}
	if id == "" {
		http.Error(w, fmt.Sprintf("container %s not found", name), http.StatusNotFound)
		return
	}

	logs, err := d.runtime.Logs(r.Context(), id, runtime.LogOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer logs.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.Copy(w, logs)
}

type eventLine struct {
	Type        string            `json:"type"`
	Name        string            `json:"name"`
	ContainerID string            `json:"container_id"`
	ExitCode    int               `json:"exit_code"`
	OOMKilled   bool              `json:"oom_killed"`
	Labels      map[string]string `json:"labels,omitempty"`
}

// handleEvents streams runtime events as newline-delimited JSON until the
// client disconnects.
func (d *Daemon) handleEvents(w http.ResponseWriter, r *http.Request) {
	events, err := d.runtime.Events(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	flusher, _ := w.(http.Flusher)
	encoder := json.NewEncoder(w)
	for event := range events {
		if err := encoder.Encode(eventLine{
			Type:        string(event.Type),
			Name:        event.Name,
			ContainerID: string(event.ContainerID),
			ExitCode:    event.ExitCode,
			OOMKilled:   event.OOMKilled,
			Labels:      event.Labels,
		}); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func (d *Daemon) handlePutDesiredState(w http.ResponseWriter, r *http.Request) {
	var desired planner.DesiredState
	if err := json.NewDecoder(r.Body).Decode(&desired); err != nil {
		http.Error(w, fmt.Sprintf("decode desired state: %v", err), http.StatusBadRequest)
		return
	}
	if desired.Service == "" || desired.Destination == "" {
		http.Error(w, "desired state requires service and destination", http.StatusBadRequest)
		return
	}
	if err := planner.ValidateDesired(desired); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := d.applySerialized(r.Context(), desired); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := d.store.SaveDesired(desired); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d.setDesired(desired)
	writeJSON(w, map[string]string{"status": "applied", "version": desired.Version})
}

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	desiredStates := d.desiredSnapshot()
	states := make([]agentstate.ActualState, 0, len(desiredStates))
	for _, desired := range desiredStates {
		actual, err := d.actualState(r.Context(), desired.Service, desired.Destination)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		states = append(states, actual)
	}
	writeJSON(w, states)
}

func (d *Daemon) handleReconcile(w http.ResponseWriter, r *http.Request) {
	d.reconcileMu.Lock()
	defer d.reconcileMu.Unlock()
	if err := d.reconcileAll(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "reconciled"})
}

func (d *Daemon) actualState(ctx context.Context, service string, destination string) (agentstate.ActualState, error) {
	containers, err := d.runtime.ListContainers(ctx, runtime.ContainerFilters{Labels: map[string]string{
		"serve.managed":     "true",
		"serve.service":     service,
		"serve.destination": destination,
	}})
	if err != nil {
		return agentstate.ActualState{}, err
	}
	actual := agentstate.ActualState{Service: service, Destination: destination}
	for _, container := range containers {
		status := "stopped"
		if container.Running {
			status = "running"
		}
		actual.Containers = append(actual.Containers, agentstate.ActualContainer{
			Name:    container.Name,
			Role:    container.Labels["serve.role"],
			Version: container.Labels["serve.version"],
			Status:  status,
		})
	}
	return actual, nil
}

func (d *Daemon) logError(format string, args ...any) {
	fmt.Fprintf(d.errorLog, "serve-agent: "+format+"\n", args...)
}

func stateKey(service string, destination string) string {
	return service + "." + destination
}

func (d *Daemon) operationLock(key string) *sync.Mutex {
	d.mu.Lock()
	defer d.mu.Unlock()
	lock := d.operationLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		d.operationLocks[key] = lock
	}
	return lock
}

func (d *Daemon) setDesired(desired planner.DesiredState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.desired[stateKey(desired.Service, desired.Destination)] = desired
}

func (d *Daemon) getDesired(key string) (planner.DesiredState, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	desired, ok := d.desired[key]
	return desired, ok
}

func (d *Daemon) desiredSnapshot() []planner.DesiredState {
	d.mu.RLock()
	defer d.mu.RUnlock()
	states := make([]planner.DesiredState, 0, len(d.desired))
	for _, desired := range d.desired {
		states = append(states, desired)
	}
	return states
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
