package healing

import (
	"context"
	"fmt"
	"time"

	"github.com/uptimenine/serve/internal/agent/proxy"
	"github.com/uptimenine/serve/internal/agent/reconciler"
	"github.com/uptimenine/serve/internal/planner"
	"github.com/uptimenine/serve/internal/runtime"
)

type Supervisor struct {
	runtime      runtime.Runtime
	reconciler   *reconciler.Reconciler
	proxyManager proxy.Manager
	eventSink    EventSink
	sleeper      Sleeper
	now          func() time.Time
	attempts     map[string]int
	lastFailure  map[string]time.Time
}

type Option func(*Supervisor)

type EventSink interface {
	Emit(ctx context.Context, event LifecycleEvent) error
}

type Sleeper interface {
	Sleep(ctx context.Context, duration time.Duration) error
}

type LifecycleEvent struct {
	Name        string
	Service     string
	Destination string
	Role        string
	Version     string
	Container   string
	ExitCode    int
	OOMKilled   bool
	Attempt     int
	Actor       string
}

type Result struct {
	Ignored         bool
	Restarted       bool
	MarkedUnhealthy bool
}

func NewSupervisor(rt runtime.Runtime, options ...Option) *Supervisor {
	s := &Supervisor{
		runtime:     rt,
		reconciler:  reconciler.New(rt),
		eventSink:   noopSink{},
		sleeper:     realSleeper{},
		now:         time.Now,
		attempts:    map[string]int{},
		lastFailure: map[string]time.Time{},
	}
	for _, option := range options {
		option(s)
	}
	return s
}

// WithReconciler replaces the supervisor's default reconciler, e.g. with one
// that can re-materialize secrets when recreating containers.
func WithReconciler(rec *reconciler.Reconciler) Option {
	return func(s *Supervisor) {
		if rec != nil {
			s.reconciler = rec
		}
	}
}

func WithProxyManager(manager proxy.Manager) Option {
	return func(s *Supervisor) {
		s.proxyManager = manager
	}
}

func WithEventSink(sink EventSink) Option {
	return func(s *Supervisor) {
		if sink != nil {
			s.eventSink = sink
		}
	}
}

func WithSleeper(sleeper Sleeper) Option {
	return func(s *Supervisor) {
		if sleeper != nil {
			s.sleeper = sleeper
		}
	}
}

func WithClock(now func() time.Time) Option {
	return func(s *Supervisor) {
		if now != nil {
			s.now = now
		}
	}
}

func (s *Supervisor) HandleRuntimeEvent(ctx context.Context, event runtime.RuntimeEvent, desired planner.DesiredState) (Result, error) {
	if !healableEvent(event.Type) {
		return Result{Ignored: true}, nil
	}

	container, ok := desiredContainer(event, desired)
	if !ok {
		return Result{Ignored: true}, nil
	}
	if !shouldRestart(container.Restart, event) {
		return Result{Ignored: true}, nil
	}

	// A failure past the restart window is a fresh incident, not part of a
	// restart loop: reset the attempt counter.
	now := s.now()
	if window, err := windowDuration(container.Restart); err != nil {
		return Result{}, err
	} else if window > 0 {
		if last, ok := s.lastFailure[container.Name]; ok && now.Sub(last) > window {
			s.attempts[container.Name] = 0
		}
	}
	s.lastFailure[container.Name] = now

	attempt := s.attempts[container.Name] + 1
	s.attempts[container.Name] = attempt
	oomKilled := event.OOMKilled || event.Type == runtime.EventOOM

	if err := s.emit(ctx, "container_exited", desired, container, event, attempt, oomKilled); err != nil {
		return Result{}, err
	}

	if container.Restart.MaxAttempts > 0 && attempt > container.Restart.MaxAttempts {
		if err := s.emit(ctx, "restart_loop_detected", desired, container, event, attempt, oomKilled); err != nil {
			return Result{}, err
		}
		return Result{MarkedUnhealthy: true}, nil
	}

	if err := s.removeProxyTarget(ctx, desired.Service, container); err != nil {
		return Result{}, err
	}
	if err := s.sleepBackoff(ctx, container.Restart, attempt); err != nil {
		return Result{}, err
	}
	if _, err := s.reconciler.Reconcile(ctx, desired); err != nil {
		return Result{}, err
	}
	if err := s.emit(ctx, "container_restarted", desired, container, event, attempt, oomKilled); err != nil {
		return Result{}, err
	}

	return Result{Restarted: true}, nil
}

func healableEvent(eventType runtime.RuntimeEventType) bool {
	switch eventType {
	case runtime.EventDie, runtime.EventStop, runtime.EventOOM:
		return true
	default:
		return false
	}
}

func desiredContainer(event runtime.RuntimeEvent, desired planner.DesiredState) (planner.Container, bool) {
	for _, container := range desired.Containers {
		if event.Name != "" && container.Name == event.Name {
			return container, true
		}
		if event.Labels["serve.role"] == container.Role && event.Labels["serve.replica"] == fmt.Sprint(container.Replica) && event.Labels["serve.version"] == desired.Version {
			return container, true
		}
	}
	return planner.Container{}, false
}

func shouldRestart(restart planner.Restart, event runtime.RuntimeEvent) bool {
	if restart.Controller == "docker" {
		return false
	}
	switch restart.Policy {
	case "", "always", "unless-stopped":
		return true
	case "on-failure":
		return event.ExitCode != 0 || event.Type == runtime.EventOOM || event.OOMKilled
	case "no":
		return false
	default:
		return false
	}
}

func (s *Supervisor) removeProxyTarget(ctx context.Context, service string, container planner.Container) error {
	if !container.Proxy || s.proxyManager == nil {
		return nil
	}
	return s.proxyManager.RemoveTarget(ctx, proxy.Target{
		Service:       service,
		Role:          container.Role,
		ContainerName: container.Name,
		Port:          targetPort(container),
		HealthPath:    targetHealthPath(container),
	})
}

func (s *Supervisor) sleepBackoff(ctx context.Context, restart planner.Restart, attempt int) error {
	backoff, err := backoffDuration(restart, attempt)
	if err != nil {
		return err
	}
	if backoff == 0 {
		return nil
	}
	return s.sleeper.Sleep(ctx, backoff)
}

func windowDuration(restart planner.Restart) (time.Duration, error) {
	if restart.Window == "" {
		return 0, nil
	}
	window, err := time.ParseDuration(restart.Window)
	if err != nil {
		return 0, fmt.Errorf("parse restart window %q: %w", restart.Window, err)
	}
	return window, nil
}

func backoffDuration(restart planner.Restart, attempt int) (time.Duration, error) {
	if restart.InitialBackoff == "" {
		return 0, nil
	}
	initial, err := time.ParseDuration(restart.InitialBackoff)
	if err != nil {
		return 0, fmt.Errorf("parse initial backoff %q: %w", restart.InitialBackoff, err)
	}
	backoff := initial
	for i := 1; i < attempt; i++ {
		backoff *= 2
	}
	if restart.MaxBackoff == "" {
		return backoff, nil
	}
	maxBackoff, err := time.ParseDuration(restart.MaxBackoff)
	if err != nil {
		return 0, fmt.Errorf("parse max backoff %q: %w", restart.MaxBackoff, err)
	}
	if backoff > maxBackoff {
		return maxBackoff, nil
	}
	return backoff, nil
}

func (s *Supervisor) emit(ctx context.Context, name string, desired planner.DesiredState, container planner.Container, event runtime.RuntimeEvent, attempt int, oomKilled bool) error {
	return s.eventSink.Emit(ctx, LifecycleEvent{
		Name:        name,
		Service:     desired.Service,
		Destination: desired.Destination,
		Role:        container.Role,
		Version:     desired.Version,
		Container:   container.Name,
		ExitCode:    event.ExitCode,
		OOMKilled:   oomKilled,
		Attempt:     attempt,
		Actor:       "serve-agent",
	})
}

func targetPort(container planner.Container) int {
	if container.Healthcheck != nil && container.Healthcheck.Port > 0 {
		return container.Healthcheck.Port
	}
	if len(container.Ports) > 0 {
		return container.Ports[0].ContainerPort
	}
	return 0
}

func targetHealthPath(container planner.Container) string {
	if container.Healthcheck != nil {
		return container.Healthcheck.Path
	}
	return ""
}

type noopSink struct{}

func (noopSink) Emit(ctx context.Context, event LifecycleEvent) error {
	_ = ctx
	_ = event
	return nil
}

type realSleeper struct{}

func (realSleeper) Sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
