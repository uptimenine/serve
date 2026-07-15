package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/uptimenine/serve/internal/agent/daemon"
	"github.com/uptimenine/serve/internal/agent/events"
	"github.com/uptimenine/serve/internal/agent/health"
	fakehealth "github.com/uptimenine/serve/internal/agent/health/fake"
	fakeproxy "github.com/uptimenine/serve/internal/agent/proxy/fake"
	agentstate "github.com/uptimenine/serve/internal/agent/state"
	"github.com/uptimenine/serve/internal/planner"
	"github.com/uptimenine/serve/internal/runtime"
	fakeruntime "github.com/uptimenine/serve/internal/runtime/fake"
)

type env struct {
	rt      *fakeruntime.Runtime
	store   *agentstate.Store
	proxy   *fakeproxy.Manager
	socket  string
	client  *http.Client
	cancel  context.CancelFunc
	stopped chan error
}

func startDaemon(t *testing.T) *env {
	return startDaemonWithHealth(t, nil)
}

func startDaemonWithHealth(t *testing.T, checker health.Checker) *env {
	t.Helper()

	dir, err := os.MkdirTemp("", "served")
	if err != nil {
		t.Fatalf("make temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	rt := fakeruntime.NewRuntime()
	stateDir := filepath.Join(dir, "state")
	store := agentstate.NewStore(stateDir)
	proxyManager := fakeproxy.NewManager()
	socket := filepath.Join(dir, "agent.sock")

	d := daemon.New(daemon.Config{
		Runtime:           rt,
		StateDir:          stateDir,
		SocketPath:        socket,
		ReconcileInterval: 20 * time.Millisecond,
		ProxyManager:      proxyManager,
		HealthChecker:     checker,
	})

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-stopped:
			if err != nil && ctx.Err() == nil {
				t.Errorf("daemon exited with error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("daemon did not stop")
		}
	})

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _ string, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socket)
		},
	}}

	e := &env{rt: rt, store: store, proxy: proxyManager, socket: socket, client: client, cancel: cancel, stopped: stopped}
	e.waitForSocket(t)
	return e
}

func (e *env) waitForSocket(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", e.socket)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("daemon socket %s never became reachable", e.socket)
}

func (e *env) waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (e *env) runningContainer(t *testing.T, name string) func() bool {
	return func() bool {
		states, err := e.rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.managed": "true"}})
		if err != nil {
			t.Fatalf("list containers: %v", err)
		}
		for _, state := range states {
			if state.Name == name && state.Running {
				return true
			}
		}
		return false
	}
}

func desiredState(version string) planner.DesiredState {
	return planner.DesiredState{
		Service:          "my-app",
		Destination:      "production",
		Host:             "localhost",
		Version:          version,
		Network:          "serve",
		RetainContainers: 5,
		Containers: []planner.Container{
			appContainer("web", version, 1),
		},
	}
}

func appContainer(role string, version string, replica int) planner.Container {
	return planner.Container{
		Name:          fmt.Sprintf("my-app-%s-production-%s-r%d", role, version, replica),
		Role:          role,
		ContainerType: "app",
		Image:         "ghcr.io/acme/my-app:" + version,
		Replica:       replica,
		Restart:       planner.Restart{Policy: "always", Controller: "agent"},
		Labels: map[string]string{
			"serve.managed":        "true",
			"serve.service":        "my-app",
			"serve.destination":    "production",
			"serve.role":           role,
			"serve.version":        version,
			"serve.replica":        strconv.Itoa(replica),
			"serve.container_type": "app",
		},
	}
}

func TestDaemonAppliesPersistedDesiredStateOnBoot(t *testing.T) {
	dir, err := os.MkdirTemp("", "served")
	if err != nil {
		t.Fatalf("make temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	stateDir := filepath.Join(dir, "state")
	if err := agentstate.NewStore(stateDir).SaveDesired(desiredState("abc123")); err != nil {
		t.Fatalf("seed desired state: %v", err)
	}

	rt := fakeruntime.NewRuntime()
	d := daemon.New(daemon.Config{
		Runtime:           rt,
		StateDir:          stateDir,
		SocketPath:        filepath.Join(dir, "agent.sock"),
		ReconcileInterval: 20 * time.Millisecond,
		ProxyManager:      fakeproxy.NewManager(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- d.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		states, err := rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.managed": "true"}})
		if err != nil {
			t.Fatalf("list containers: %v", err)
		}
		for _, state := range states {
			if state.Name == "my-app-web-production-abc123-r1" && state.Running {
				cancel()
				<-stopped
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("daemon never applied persisted desired state")
}

func TestDaemonAppliesDesiredStateOverSocket(t *testing.T) {
	e := startDaemon(t)

	body, err := json.Marshal(desiredState("abc123"))
	if err != nil {
		t.Fatalf("marshal desired state: %v", err)
	}
	request, err := http.NewRequest(http.MethodPut, "http://serve-agent/v1/desired-state", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	response, err := e.client.Do(request)
	if err != nil {
		t.Fatalf("PUT desired-state: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("PUT desired-state status = %d", response.StatusCode)
	}

	e.waitFor(t, "container from socket apply", e.runningContainer(t, "my-app-web-production-abc123-r1"))

	persisted, err := e.store.LoadDesired("my-app", "production")
	if err != nil {
		t.Fatalf("desired state must be persisted for restarts: %v", err)
	}
	if persisted.Version != "abc123" {
		t.Fatalf("persisted version = %q, want abc123", persisted.Version)
	}
}

func TestFailedDesiredStateDoesNotReplaceActiveStateOrHealingTarget(t *testing.T) {
	checker := fakehealth.NewChecker()
	e := startDaemonWithHealth(t, checker)
	active := desiredState("abc123")
	active.Containers[0].Proxy = true
	active.Containers[0].Ports = []planner.Port{{Name: "http", ContainerPort: 3000}}
	active.Containers[0].Healthcheck = &planner.Healthcheck{Type: "http", Path: "/up", Port: 3000, Retries: 1}
	checker.SetStatus(active.Containers[0].Name, health.Healthy)
	putDesiredState(t, e, active, http.StatusOK)

	candidate := desiredState("def456")
	candidate.Containers[0].Proxy = true
	candidate.Containers[0].Ports = []planner.Port{{Name: "http", ContainerPort: 3000}}
	candidate.Containers[0].Healthcheck = active.Containers[0].Healthcheck
	checker.SetStatus(candidate.Containers[0].Name, health.Unhealthy)
	putDesiredState(t, e, candidate, http.StatusInternalServerError)

	persisted, err := e.store.LoadDesired("my-app", "production")
	if err != nil {
		t.Fatalf("load active desired state: %v", err)
	}
	if persisted.Version != "abc123" {
		t.Fatalf("failed candidate replaced active desired version with %q", persisted.Version)
	}

	states, err := e.rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.version": "abc123"}})
	if err != nil || len(states) != 1 {
		t.Fatalf("find active container: %v %#v", err, states)
	}
	if err := e.rt.StopContainer(context.Background(), states[0].ID, time.Second); err != nil {
		t.Fatalf("stop active container: %v", err)
	}
	e.waitFor(t, "active version to heal", e.runningContainer(t, active.Containers[0].Name))
}

func TestReconcileEndpointReportsApplyFailures(t *testing.T) {
	checker := fakehealth.NewChecker()
	e := startDaemonWithHealth(t, checker)
	desired := desiredState("bad123")
	desired.Containers[0].Proxy = true
	desired.Containers[0].Ports = []planner.Port{{Name: "http", ContainerPort: 3000}}
	desired.Containers[0].Healthcheck = &planner.Healthcheck{Type: "http", Path: "/up", Port: 3000, Retries: 1}
	checker.SetStatus(desired.Containers[0].Name, health.Unhealthy)
	if err := e.store.SaveDesired(desired); err != nil {
		t.Fatalf("save desired state: %v", err)
	}

	response, err := e.client.Post("http://serve-agent/v1/reconcile", "application/json", nil)
	if err != nil {
		t.Fatalf("POST reconcile: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("POST reconcile status = %d, want 500", response.StatusCode)
	}
}

func TestFailedDiskReconcilePreservesActiveHealingTarget(t *testing.T) {
	checker := fakehealth.NewChecker()
	e := startDaemonWithHealth(t, checker)
	active := proxiedDesiredState("abc123")
	checker.SetStatus(active.Containers[0].Name, health.Healthy)
	putDesiredState(t, e, active, http.StatusOK)

	candidate := proxiedDesiredState("def456")
	checker.SetStatus(candidate.Containers[0].Name, health.Unhealthy)
	if err := e.store.SaveDesired(candidate); err != nil {
		t.Fatalf("save candidate desired state: %v", err)
	}
	response, err := e.client.Post("http://serve-agent/v1/reconcile", "application/json", nil)
	if err != nil {
		t.Fatalf("POST reconcile: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("POST reconcile status = %d, want 500", response.StatusCode)
	}

	states, err := e.rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.version": "abc123"}})
	if err != nil || len(states) != 1 {
		t.Fatalf("find active container: %v %#v", err, states)
	}
	if err := e.rt.StopContainer(context.Background(), states[0].ID, time.Second); err != nil {
		t.Fatalf("stop active container: %v", err)
	}
	e.waitFor(t, "active version to heal after failed disk reconcile", e.runningContainer(t, active.Containers[0].Name))
}

func TestSlowReconcileDoesNotBlockStatusAPI(t *testing.T) {
	checker := newBlockingHealthChecker()
	e := startDaemonWithHealth(t, checker)
	desired := proxiedDesiredState("slow123")
	if err := e.store.SaveDesired(desired); err != nil {
		t.Fatalf("save desired state: %v", err)
	}

	reconcileDone := make(chan *http.Response, 1)
	go func() {
		response, _ := e.client.Post("http://serve-agent/v1/reconcile", "application/json", nil)
		reconcileDone <- response
	}()
	select {
	case <-checker.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("health check did not start")
	}
	defer close(checker.release)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://serve-agent/v1/status", nil)
	if err != nil {
		t.Fatalf("new status request: %v", err)
	}
	response, err := e.client.Do(request)
	if err != nil {
		t.Fatalf("status blocked behind slow reconcile: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d", response.StatusCode)
	}
}

func TestSlowReconcileDoesNotBlockHealingAnotherService(t *testing.T) {
	slow := proxiedDesiredState("slow123")
	checker := newSelectiveBlockingHealthChecker(slow.Containers[0].Name)
	e := startDaemonWithHealth(t, checker)

	other := desiredState("stable123")
	other.Service = "other-app"
	other.Containers[0].Name = "other-app-web-production-stable123-r1"
	other.Containers[0].Labels["serve.service"] = "other-app"
	putDesiredState(t, e, other, http.StatusOK)

	if err := e.store.SaveDesired(slow); err != nil {
		t.Fatalf("save slow desired state: %v", err)
	}
	select {
	case <-checker.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("slow health check did not start")
	}
	defer close(checker.release)

	states, err := e.rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.service": "other-app"}})
	if err != nil || len(states) != 1 {
		t.Fatalf("find other service container: %v %#v", err, states)
	}
	if err := e.rt.StopContainer(context.Background(), states[0].ID, time.Second); err != nil {
		t.Fatalf("stop other service: %v", err)
	}

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if e.runningContainer(t, other.Containers[0].Name)() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("other service was not healed while slow reconcile was blocked")
}

func proxiedDesiredState(version string) planner.DesiredState {
	desired := desiredState(version)
	desired.Containers[0].Proxy = true
	desired.Containers[0].Ports = []planner.Port{{Name: "http", ContainerPort: 3000}}
	desired.Containers[0].Healthcheck = &planner.Healthcheck{Type: "http", Path: "/up", Port: 3000, Retries: 1}
	return desired
}

type blockingHealthChecker struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	name    string
}

func newBlockingHealthChecker() *blockingHealthChecker {
	return &blockingHealthChecker{entered: make(chan struct{}), release: make(chan struct{})}
}

func newSelectiveBlockingHealthChecker(name string) *blockingHealthChecker {
	return &blockingHealthChecker{entered: make(chan struct{}), release: make(chan struct{}), name: name}
}

func (c *blockingHealthChecker) Check(ctx context.Context, target health.Target) (health.Status, error) {
	if c.name != "" && target.ContainerName != c.name {
		return health.Healthy, nil
	}
	c.once.Do(func() {
		close(c.entered)
		select {
		case <-c.release:
		case <-ctx.Done():
		}
	})
	if err := ctx.Err(); err != nil {
		return health.Unhealthy, err
	}
	return health.Healthy, nil
}

func putDesiredState(t *testing.T, e *env, desired planner.DesiredState, wantStatus int) {
	t.Helper()
	body, err := json.Marshal(desired)
	if err != nil {
		t.Fatalf("marshal desired state: %v", err)
	}
	request, err := http.NewRequest(http.MethodPut, "http://serve-agent/v1/desired-state", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	response, err := e.client.Do(request)
	if err != nil {
		t.Fatalf("PUT desired-state: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != wantStatus {
		contents, _ := io.ReadAll(response.Body)
		t.Fatalf("PUT desired-state status = %d, want %d: %s", response.StatusCode, wantStatus, contents)
	}
}

func TestDaemonStatusEndpointReportsContainers(t *testing.T) {
	e := startDaemon(t)

	if err := e.store.SaveDesired(desiredState("abc123")); err != nil {
		t.Fatalf("save desired state: %v", err)
	}
	response, err := e.client.Post("http://serve-agent/v1/reconcile", "application/json", nil)
	if err != nil {
		t.Fatalf("POST reconcile: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST reconcile status = %d", response.StatusCode)
	}
	e.waitFor(t, "container from reconcile", e.runningContainer(t, "my-app-web-production-abc123-r1"))

	statusResponse, err := e.client.Get("http://serve-agent/v1/status")
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	defer statusResponse.Body.Close()
	if statusResponse.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d", statusResponse.StatusCode)
	}
	var status []agentstate.ActualState
	if err := json.NewDecoder(statusResponse.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if len(status) != 1 || status[0].Service != "my-app" {
		t.Fatalf("unexpected status payload: %#v", status)
	}
	if len(status[0].Containers) != 1 || status[0].Containers[0].Name != "my-app-web-production-abc123-r1" {
		t.Fatalf("unexpected status containers: %#v", status[0].Containers)
	}
}

func TestDaemonHealsExitedContainerFromEvent(t *testing.T) {
	e := startDaemon(t)

	if err := e.store.SaveDesired(desiredState("abc123")); err != nil {
		t.Fatalf("save desired state: %v", err)
	}
	e.waitFor(t, "initial apply", e.runningContainer(t, "my-app-web-production-abc123-r1"))

	states, err := e.rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.role": "web"}})
	if err != nil || len(states) == 0 {
		t.Fatalf("find web container: %v %#v", err, states)
	}
	e.rt.Die(states[0].ID, 137, false)

	e.waitFor(t, "healed container", e.runningContainer(t, "my-app-web-production-abc123-r1"))
}

func TestDaemonLogsEndpointStreamsContainerLogs(t *testing.T) {
	e := startDaemon(t)

	if err := e.store.SaveDesired(desiredState("abc123")); err != nil {
		t.Fatalf("save desired state: %v", err)
	}
	e.waitFor(t, "initial apply", e.runningContainer(t, "my-app-web-production-abc123-r1"))

	states, err := e.rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.role": "web"}})
	if err != nil || len(states) == 0 {
		t.Fatalf("find web container: %v %#v", err, states)
	}
	e.rt.SetLogs(states[0].ID, "hello from container\n")

	response, err := e.client.Get("http://serve-agent/v1/logs?container=my-app-web-production-abc123-r1")
	if err != nil {
		t.Fatalf("GET logs: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET logs status = %d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read logs body: %v", err)
	}
	if string(body) != "hello from container\n" {
		t.Fatalf("logs body = %q", body)
	}

	missing, err := e.client.Get("http://serve-agent/v1/logs?container=nope")
	if err != nil {
		t.Fatalf("GET missing logs: %v", err)
	}
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing container status = %d, want 404", missing.StatusCode)
	}
}

func TestDaemonEventsEndpointStreamsRuntimeEvents(t *testing.T) {
	e := startDaemon(t)

	if err := e.store.SaveDesired(desiredState("abc123")); err != nil {
		t.Fatalf("save desired state: %v", err)
	}
	e.waitFor(t, "initial apply", e.runningContainer(t, "my-app-web-production-abc123-r1"))

	type line struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	lines := make(chan line, 1)
	errs := make(chan error, 1)
	go func() {
		response, err := e.client.Get("http://serve-agent/v1/events")
		if err != nil {
			errs <- err
			return
		}
		defer response.Body.Close()
		var decoded line
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
			errs <- err
			return
		}
		lines <- decoded
	}()

	// Give the events subscription a moment to attach, then kill the
	// container so an event flows.
	e.rt.WaitForSubscribers(t, 2) // daemon loop + events endpoint
	states, err := e.rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.role": "web"}})
	if err != nil || len(states) == 0 {
		t.Fatalf("find web container: %v %#v", err, states)
	}
	e.rt.Die(states[0].ID, 137, false)

	select {
	case decoded := <-lines:
		if decoded.Type != "die" || decoded.Name != "my-app-web-production-abc123-r1" {
			t.Fatalf("unexpected event line: %#v", decoded)
		}
	case err := <-errs:
		t.Fatalf("events request failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for streamed event")
	}
}

func TestDaemonEmitsStructuredJSONEventsOnHeal(t *testing.T) {
	log := &syncBuffer{}

	dir, err := os.MkdirTemp("", "served")
	if err != nil {
		t.Fatalf("make temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	rt := fakeruntime.NewRuntime()
	stateDir := filepath.Join(dir, "state")
	store := agentstate.NewStore(stateDir)
	if err := store.SaveDesired(desiredState("abc123")); err != nil {
		t.Fatalf("seed desired state: %v", err)
	}

	d := daemon.New(daemon.Config{
		Runtime:           rt,
		StateDir:          stateDir,
		SocketPath:        filepath.Join(dir, "agent.sock"),
		ReconcileInterval: 20 * time.Millisecond,
		ProxyManager:      fakeproxy.NewManager(),
		EventSink:         events.NewJSONSink(log),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- d.Run(ctx) }()

	e := &env{rt: rt, store: store}
	e.waitFor(t, "initial apply", e.runningContainer(t, "my-app-web-production-abc123-r1"))

	states, err := rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.role": "web"}})
	if err != nil || len(states) == 0 {
		t.Fatalf("find web container: %v %#v", err, states)
	}
	rt.Die(states[0].ID, 137, true)
	e.waitFor(t, "healed container", e.runningContainer(t, "my-app-web-production-abc123-r1"))

	e.waitFor(t, "restart event in log", func() bool {
		return bytes.Contains(log.Bytes(), []byte("container_restarted"))
	})
	for _, line := range bytes.Split(bytes.TrimSpace(log.Bytes()), []byte("\n")) {
		var decoded map[string]any
		if err := json.Unmarshal(line, &decoded); err != nil {
			t.Fatalf("event log line is not JSON: %v\n%s", err, line)
		}
		if decoded["service"] != "my-app" || decoded["container"] != "my-app-web-production-abc123-r1" {
			t.Fatalf("event line missing identity labels: %s", line)
		}
	}

	cancel()
	<-stopped
}

type syncBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buffer.Bytes()...)
}

func TestDaemonReconcilesRemovedContainerWithoutEvent(t *testing.T) {
	e := startDaemon(t)

	if err := e.store.SaveDesired(desiredState("abc123")); err != nil {
		t.Fatalf("save desired state: %v", err)
	}
	e.waitFor(t, "initial apply", e.runningContainer(t, "my-app-web-production-abc123-r1"))

	states, err := e.rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.role": "web"}})
	if err != nil || len(states) == 0 {
		t.Fatalf("find web container: %v %#v", err, states)
	}
	// Removal emits a destroy event, which the supervisor ignores; only the
	// periodic reconcile can repair this.
	if err := e.rt.RemoveContainer(context.Background(), states[0].ID); err != nil {
		t.Fatalf("remove container: %v", err)
	}

	e.waitFor(t, "reconciled container", e.runningContainer(t, "my-app-web-production-abc123-r1"))
}
