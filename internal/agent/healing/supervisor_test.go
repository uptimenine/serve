package healing_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/uptimenine/serve/internal/agent/healing"
	"github.com/uptimenine/serve/internal/agent/proxy"
	"github.com/uptimenine/serve/internal/planner"
	"github.com/uptimenine/serve/internal/runtime"
	"github.com/uptimenine/serve/internal/runtime/fake"
)

func TestHandleDieRestartsDesiredContainerAndLogsRestart(t *testing.T) {
	rt := fake.NewRuntime()
	desired := desiredState("abc123")
	seedDesiredContainer(t, rt, desired)
	container := runningContainer(t, rt)
	rt.Die(container.ID, 137, true)
	sink := &recordingSink{}
	supervisor := healing.NewSupervisor(rt, healing.WithEventSink(sink), healing.WithSleeper(&recordingSleeper{}))

	result, err := supervisor.HandleRuntimeEvent(context.Background(), dieEvent(container, 137, true), desired)

	if err != nil {
		t.Fatalf("handle die event: %v", err)
	}
	if !result.Restarted {
		t.Fatalf("expected container to be restarted, got %#v", result)
	}
	restarted := runningContainer(t, rt)
	if restarted.ID == container.ID {
		t.Fatalf("expected stopped container to be recreated with a new ID, still had %s", restarted.ID)
	}
	if restarted.Name != container.Name || !restarted.Running {
		t.Fatalf("expected replacement container running with same name, got %#v", restarted)
	}

	restartEvent := sink.last("container_restarted")
	if restartEvent.Name != "container_restarted" {
		t.Fatalf("expected container_restarted event, got %#v", sink.events)
	}
	if restartEvent.Container != container.Name || restartEvent.ExitCode != 137 || !restartEvent.OOMKilled {
		t.Fatalf("expected exit/OOM details in restart event, got %#v", restartEvent)
	}
	if restartEvent.Attempt != 1 || restartEvent.Actor != "serve-agent" {
		t.Fatalf("expected attempt and actor details, got %#v", restartEvent)
	}
}

func TestHandleDieRemovesWebProxyTargetBeforeRestart(t *testing.T) {
	rt := fake.NewRuntime()
	desired := desiredState("abc123")
	desired.Containers[0].Proxy = true
	seedDesiredContainer(t, rt, desired)
	container := runningContainer(t, rt)
	rt.Die(container.ID, 1, false)
	rt.ClearOperations()
	manager := &orderAssertingProxy{t: t, rt: rt}
	supervisor := healing.NewSupervisor(rt, healing.WithProxyManager(manager), healing.WithSleeper(&recordingSleeper{}))

	_, err := supervisor.HandleRuntimeEvent(context.Background(), dieEvent(container, 1, false), desired)

	if err != nil {
		t.Fatalf("handle die event: %v", err)
	}
	if manager.removeCount != 1 {
		t.Fatalf("expected one proxy target removal, got %d", manager.removeCount)
	}
}

func TestRepeatedFailuresApplyExponentialBackoff(t *testing.T) {
	rt := fake.NewRuntime()
	desired := desiredState("abc123")
	desired.Containers[0].Restart = planner.Restart{
		Policy:         "always",
		Controller:     "agent",
		InitialBackoff: "2s",
		MaxBackoff:     "5s",
		MaxAttempts:    5,
	}
	seedDesiredContainer(t, rt, desired)
	sleeper := &recordingSleeper{}
	supervisor := healing.NewSupervisor(rt, healing.WithSleeper(sleeper))

	for range 3 {
		container := runningContainer(t, rt)
		rt.Die(container.ID, 1, false)
		if _, err := supervisor.HandleRuntimeEvent(context.Background(), dieEvent(container, 1, false), desired); err != nil {
			t.Fatalf("handle die event: %v", err)
		}
	}

	expected := []time.Duration{2 * time.Second, 4 * time.Second, 5 * time.Second}
	if len(sleeper.durations) != len(expected) {
		t.Fatalf("expected backoff durations %#v, got %#v", expected, sleeper.durations)
	}
	for i := range expected {
		if sleeper.durations[i] != expected[i] {
			t.Fatalf("expected backoff durations %#v, got %#v", expected, sleeper.durations)
		}
	}
}

func TestExceedingMaxAttemptsMarksContainerUnhealthyAndDoesNotRestart(t *testing.T) {
	rt := fake.NewRuntime()
	desired := desiredState("abc123")
	desired.Containers[0].Restart = planner.Restart{Policy: "always", Controller: "agent", MaxAttempts: 1}
	seedDesiredContainer(t, rt, desired)
	sink := &recordingSink{}
	supervisor := healing.NewSupervisor(rt, healing.WithEventSink(sink), healing.WithSleeper(&recordingSleeper{}))

	first := runningContainer(t, rt)
	rt.Die(first.ID, 1, false)
	if _, err := supervisor.HandleRuntimeEvent(context.Background(), dieEvent(first, 1, false), desired); err != nil {
		t.Fatalf("handle first die event: %v", err)
	}
	second := runningContainer(t, rt)
	rt.Die(second.ID, 1, false)
	rt.ClearOperations()

	result, err := supervisor.HandleRuntimeEvent(context.Background(), dieEvent(second, 1, false), desired)

	if err != nil {
		t.Fatalf("handle second die event: %v", err)
	}
	if !result.MarkedUnhealthy || result.Restarted {
		t.Fatalf("expected restart loop to be marked unhealthy without restart, got %#v", result)
	}
	for _, operation := range rt.Operations() {
		if strings.HasPrefix(operation, "create_container:") || strings.HasPrefix(operation, "start_container:") {
			t.Fatalf("expected no restart after max attempts exceeded, got operations %#v", rt.Operations())
		}
	}
	loopEvent := sink.last("restart_loop_detected")
	if loopEvent.Name != "restart_loop_detected" || loopEvent.Attempt != 2 {
		t.Fatalf("expected restart_loop_detected event for attempt 2, got %#v", sink.events)
	}
}

func TestFailuresOutsideRestartWindowResetAttemptCounter(t *testing.T) {
	rt := fake.NewRuntime()
	desired := desiredState("abc123")
	desired.Containers[0].Restart = planner.Restart{Policy: "always", Controller: "agent", MaxAttempts: 1, Window: "1m"}
	seedDesiredContainer(t, rt, desired)

	clock := &fakeClock{current: time.Now()}
	supervisor := healing.NewSupervisor(rt, healing.WithSleeper(&recordingSleeper{}), healing.WithClock(clock.Now))

	first := runningContainer(t, rt)
	rt.Die(first.ID, 1, false)
	result, err := supervisor.HandleRuntimeEvent(context.Background(), dieEvent(first, 1, false), desired)
	if err != nil {
		t.Fatalf("handle first die event: %v", err)
	}
	if !result.Restarted {
		t.Fatalf("expected first failure to restart, got %#v", result)
	}

	// A second failure long after the window must count as a fresh incident
	// rather than exceeding max_attempts.
	clock.current = clock.current.Add(5 * time.Minute)
	second := runningContainer(t, rt)
	rt.Die(second.ID, 1, false)
	result, err = supervisor.HandleRuntimeEvent(context.Background(), dieEvent(second, 1, false), desired)
	if err != nil {
		t.Fatalf("handle second die event: %v", err)
	}
	if !result.Restarted || result.MarkedUnhealthy {
		t.Fatalf("expected failure outside window to restart with reset attempts, got %#v", result)
	}
}

type fakeClock struct {
	current time.Time
}

func (c *fakeClock) Now() time.Time {
	return c.current
}

func desiredState(version string) planner.DesiredState {
	name := "my-app-web-production-" + version + "-r1"
	return planner.DesiredState{
		Service:          "my-app",
		Destination:      "production",
		Host:             "app1.example.com",
		Version:          version,
		Network:          "serve",
		RetainContainers: 5,
		Containers: []planner.Container{
			{
				Name:          name,
				Role:          "web",
				ContainerType: "app",
				Image:         "ghcr.io/acme/my-app:" + version,
				Command:       []string{"./server"},
				Ports:         []planner.Port{{Name: "http", ContainerPort: 3000}},
				Replica:       1,
				Proxy:         true,
				Healthcheck:   &planner.Healthcheck{Type: "http", Path: "/up", Port: 3000},
				Restart:       planner.Restart{Policy: "always", Controller: "agent"},
				Labels: map[string]string{
					"serve.managed":        "true",
					"serve.service":        "my-app",
					"serve.destination":    "production",
					"serve.role":           "web",
					"serve.version":        version,
					"serve.replica":        "1",
					"serve.container_type": "app",
				},
			},
		},
	}
}

func seedDesiredContainer(t *testing.T, rt *fake.Runtime, desired planner.DesiredState) {
	t.Helper()
	if _, err := rt.CreateContainer(context.Background(), runtime.ContainerSpec{
		Name:    desired.Containers[0].Name,
		Image:   desired.Containers[0].Image,
		Command: desired.Containers[0].Command,
		Labels:  desired.Containers[0].Labels,
	}); err != nil {
		t.Fatalf("seed container: %v", err)
	}
	containers, err := rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.managed": "true"}})
	if err != nil {
		t.Fatalf("list seeded container: %v", err)
	}
	if err := rt.StartContainer(context.Background(), containers[0].ID); err != nil {
		t.Fatalf("start seeded container: %v", err)
	}
}

func runningContainer(t *testing.T, rt *fake.Runtime) runtime.ContainerState {
	t.Helper()
	containers, err := rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.managed": "true"}})
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	for _, container := range containers {
		if container.Running {
			return container
		}
	}
	t.Fatalf("expected one running container, got %#v", containers)
	return runtime.ContainerState{}
}

func dieEvent(container runtime.ContainerState, exitCode int, oomKilled bool) runtime.RuntimeEvent {
	return runtime.RuntimeEvent{
		Type:        runtime.EventDie,
		ContainerID: container.ID,
		Name:        container.Name,
		Labels:      container.Labels,
		ExitCode:    exitCode,
		OOMKilled:   oomKilled,
	}
}

type recordingSink struct {
	events []healing.LifecycleEvent
}

func (s *recordingSink) Emit(ctx context.Context, event healing.LifecycleEvent) error {
	_ = ctx
	s.events = append(s.events, event)
	return nil
}

func (s *recordingSink) last(name string) healing.LifecycleEvent {
	for i := len(s.events) - 1; i >= 0; i-- {
		if s.events[i].Name == name {
			return s.events[i]
		}
	}
	return healing.LifecycleEvent{}
}

type recordingSleeper struct {
	durations []time.Duration
}

func (s *recordingSleeper) Sleep(ctx context.Context, duration time.Duration) error {
	_ = ctx
	s.durations = append(s.durations, duration)
	return nil
}

type orderAssertingProxy struct {
	t           *testing.T
	rt          *fake.Runtime
	removeCount int
}

func (p *orderAssertingProxy) AddTarget(ctx context.Context, target proxy.Target) error {
	_ = ctx
	_ = target
	return nil
}

func (p *orderAssertingProxy) SetTargets(ctx context.Context, service string, role string, targets []proxy.Target, opts proxy.RouteOptions) error {
	_ = ctx
	_ = service
	_ = role
	_ = targets
	_ = opts
	return nil
}

func (p *orderAssertingProxy) RemoveTarget(ctx context.Context, target proxy.Target) error {
	_ = ctx
	_ = target
	p.removeCount++
	for _, operation := range p.rt.Operations() {
		if strings.HasPrefix(operation, "create_container:") || strings.HasPrefix(operation, "start_container:") {
			p.t.Fatalf("proxy target was removed after restart began; operations at removal: %#v", p.rt.Operations())
		}
	}
	return nil
}
