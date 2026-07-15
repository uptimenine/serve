package cutover_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uptimenine/serve/internal/agent/cutover"
	"github.com/uptimenine/serve/internal/agent/health"
	fakehealth "github.com/uptimenine/serve/internal/agent/health/fake"
	fakeproxy "github.com/uptimenine/serve/internal/agent/proxy/fake"
	"github.com/uptimenine/serve/internal/agent/reconciler"
	agentstate "github.com/uptimenine/serve/internal/agent/state"
	"github.com/uptimenine/serve/internal/planner"
	"github.com/uptimenine/serve/internal/runtime"
	fakeruntime "github.com/uptimenine/serve/internal/runtime/fake"
)

func desiredState(version string, webReplicas int, withWorker bool) planner.DesiredState {
	state := planner.DesiredState{
		Service:          "my-app",
		Destination:      "production",
		Host:             "app1.example.com",
		Version:          version,
		Network:          "serve",
		RetainContainers: 5,
	}
	for replica := 1; replica <= webReplicas; replica++ {
		state.Containers = append(state.Containers, appContainer("web", version, replica, true))
	}
	if withWorker {
		state.Containers = append(state.Containers, appContainer("worker", version, 1, false))
	}
	return state
}

func appContainer(role string, version string, replica int, proxied bool) planner.Container {
	container := planner.Container{
		Name:          fmt.Sprintf("my-app-%s-production-%s-r%d", role, version, replica),
		Role:          role,
		ContainerType: "app",
		Image:         "ghcr.io/acme/my-app:" + version,
		Replica:       replica,
		Proxy:         proxied,
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
	if proxied {
		container.Ports = []planner.Port{{Name: "http", ContainerPort: 3000}}
		container.Healthcheck = &planner.Healthcheck{Type: "http", Path: "/up", Port: 3000, Interval: "1ms", Retries: 3}
	}
	return container
}

type env struct {
	rt      *fakeruntime.Runtime
	checker *fakehealth.Checker
	proxy   *fakeproxy.Manager
	store   *agentstate.Store
	engine  *cutover.Engine
}

func newEnv(t *testing.T) *env {
	t.Helper()
	rt := fakeruntime.NewRuntime()
	checker := fakehealth.NewChecker()
	proxyManager := fakeproxy.NewManager()
	store := agentstate.NewStore(t.TempDir())
	engine := cutover.New(cutover.Deps{
		Runtime:  rt,
		Starter:  reconciler.New(rt),
		Health:   checker,
		Proxy:    proxyManager,
		LastGood: store,
		Sleeper:  noopSleeper{},
	})
	return &env{rt: rt, checker: checker, proxy: proxyManager, store: store, engine: engine}
}

// deploy runs a full healthy deploy of version so tests can start from a
// serving old version.
func (e *env) deploy(t *testing.T, desired planner.DesiredState) {
	t.Helper()
	for _, container := range desired.Containers {
		e.checker.SetStatus(container.Name, health.Healthy)
	}
	if err := e.engine.Apply(context.Background(), desired); err != nil {
		t.Fatalf("deploy %s: %v", desired.Version, err)
	}
}

func (e *env) containersByVersion(t *testing.T, version string) []runtime.ContainerState {
	t.Helper()
	states, err := e.rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{
		"serve.managed": "true",
		"serve.version": version,
	}})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	return states
}

func TestHealthyCandidateCutsOverAndStopsOldVersion(t *testing.T) {
	env := newEnv(t)
	env.deploy(t, desiredState("abc123", 2, true))

	desired := desiredState("def456", 2, true)
	for _, container := range desired.Containers {
		env.checker.SetStatus(container.Name, health.Healthy)
	}
	if err := env.engine.Apply(context.Background(), desired); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	targets := env.proxy.Targets()
	if len(targets) != 2 {
		t.Fatalf("expected 2 routed targets after cutover, got %+v", targets)
	}
	for _, target := range targets {
		if target.ContainerName != "my-app-web-production-def456-r1" && target.ContainerName != "my-app-web-production-def456-r2" {
			t.Fatalf("old version still routed: %+v", target)
		}
	}

	for _, state := range env.containersByVersion(t, "abc123") {
		if state.Running {
			t.Fatalf("old container %s still running after cutover", state.Name)
		}
	}
	for _, state := range env.containersByVersion(t, "def456") {
		if !state.Running {
			t.Fatalf("new container %s not running", state.Name)
		}
	}

	lastGood, err := env.store.LoadLastGood("my-app", "production")
	if err != nil {
		t.Fatalf("LoadLastGood: %v", err)
	}
	if lastGood.Version != "def456" {
		t.Fatalf("last-good version = %q, want def456", lastGood.Version)
	}
}

func TestUnhealthyCandidateKeepsOldVersionServing(t *testing.T) {
	env := newEnv(t)
	env.deploy(t, desiredState("abc123", 1, false))

	desired := desiredState("def456", 1, false)
	env.checker.SetStatus("my-app-web-production-def456-r1", health.Unhealthy)

	err := env.engine.Apply(context.Background(), desired)
	if !errors.Is(err, cutover.ErrCandidateUnhealthy) {
		t.Fatalf("Apply error = %v, want ErrCandidateUnhealthy", err)
	}

	targets := env.proxy.Targets()
	if len(targets) != 1 || targets[0].ContainerName != "my-app-web-production-abc123-r1" {
		t.Fatalf("old version must keep serving, routed targets: %+v", targets)
	}

	old := env.containersByVersion(t, "abc123")
	if len(old) != 1 || !old[0].Running {
		t.Fatalf("old container must stay running, got %+v", old)
	}
	if candidates := env.containersByVersion(t, "def456"); len(candidates) != 0 {
		t.Fatalf("failed candidates must be cleaned up, got %+v", candidates)
	}

	if _, err := env.store.LoadLastGood("my-app", "production"); err == nil {
		lastGood, _ := env.store.LoadLastGood("my-app", "production")
		if lastGood.Version == "def456" {
			t.Fatalf("failed deploy must not be recorded as last-good")
		}
	}
}

func TestCandidateStartupFailureCleansUpContainersStartedEarlier(t *testing.T) {
	env := newEnv(t)
	starter := &failingStarter{delegate: reconciler.New(env.rt), failAt: 2}
	engine := cutover.New(cutover.Deps{
		Runtime: env.rt, Starter: starter, Health: env.checker,
		Proxy: env.proxy, LastGood: env.store, Sleeper: noopSleeper{},
	})
	desired := desiredState("abc123", 2, false)

	err := engine.Apply(context.Background(), desired)

	if err == nil || !strings.Contains(err.Error(), "start failed") {
		t.Fatalf("Apply error = %v, want startup failure", err)
	}
	if candidates := env.containersByVersion(t, "abc123"); len(candidates) != 0 {
		t.Fatalf("partially started candidates were not cleaned up: %+v", candidates)
	}
}

type failingStarter struct {
	delegate *reconciler.Reconciler
	calls    int
	failAt   int
}

func (s *failingStarter) EnsureContainer(ctx context.Context, desired planner.DesiredState, container planner.Container) (reconciler.EnsureResult, error) {
	s.calls++
	if s.calls == s.failAt {
		return reconciler.EnsureResult{}, errors.New("start failed")
	}
	return s.delegate.EnsureContainer(ctx, desired, container)
}

func TestDependentRolesBootOnlyAfterPrimaryRoleHealthy(t *testing.T) {
	env := newEnv(t)

	desired := desiredState("abc123", 1, true)
	env.checker.SetStatus("my-app-web-production-abc123-r1", health.Unhealthy)

	err := env.engine.Apply(context.Background(), desired)
	if !errors.Is(err, cutover.ErrCandidateUnhealthy) {
		t.Fatalf("Apply error = %v, want ErrCandidateUnhealthy", err)
	}

	for _, state := range env.containersByVersion(t, "abc123") {
		if state.Labels["serve.role"] == "worker" {
			t.Fatalf("worker booted before primary role became healthy")
		}
	}
}

func TestRetentionKeepsOnlyConfiguredVersions(t *testing.T) {
	env := newEnv(t)

	versions := []string{"v1", "v2", "v3", "v4"}
	for _, version := range versions {
		desired := desiredState(version, 1, false)
		desired.RetainContainers = 2
		env.deploy(t, desired)
	}

	if running := env.containersByVersion(t, "v4"); len(running) != 1 || !running[0].Running {
		t.Fatalf("current version must run, got %+v", running)
	}
	if retained := env.containersByVersion(t, "v3"); len(retained) != 1 || retained[0].Running {
		t.Fatalf("previous version must be retained stopped, got %+v", retained)
	}
	for _, version := range []string{"v1", "v2"} {
		if stale := env.containersByVersion(t, version); len(stale) != 0 {
			t.Fatalf("version %s must be pruned per retain_containers, got %+v", version, stale)
		}
	}
}

func TestCandidateBecomesHealthyAfterPolling(t *testing.T) {
	env := newEnv(t)

	checker := &flakyChecker{healthyAfter: 3}
	engine := cutover.New(cutover.Deps{
		Runtime:  env.rt,
		Starter:  reconciler.New(env.rt),
		Health:   checker,
		Proxy:    env.proxy,
		LastGood: env.store,
		Sleeper:  noopSleeper{},
	})

	if err := engine.Apply(context.Background(), desiredState("abc123", 1, false)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if checker.checks < 3 {
		t.Fatalf("expected health to be polled until healthy, checks = %d", checker.checks)
	}
	if targets := env.proxy.Targets(); len(targets) != 1 {
		t.Fatalf("expected candidate to be routed once healthy, targets: %+v", targets)
	}
}

func TestSwitchTrafficPassesProxyRouteOptions(t *testing.T) {
	env := newEnv(t)

	desired := desiredState("abc123", 1, false)
	desired.Proxy = planner.ProxyRoute{
		Hosts:         []string{"app.example.com"},
		SSL:           true,
		DeployTimeout: "45s",
		DrainTimeout:  "60s",
	}
	env.checker.SetStatus("my-app-web-production-abc123-r1", health.Healthy)

	if err := env.engine.Apply(context.Background(), desired); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	opts := env.proxy.LastRouteOptions()
	if len(opts.Hosts) != 1 || opts.Hosts[0] != "app.example.com" {
		t.Fatalf("route hosts = %#v", opts.Hosts)
	}
	if !opts.TLS {
		t.Fatalf("expected TLS route option, got %#v", opts)
	}
	if opts.DeployTimeout != 45*time.Second || opts.DrainTimeout != 60*time.Second {
		t.Fatalf("route timeouts = %v/%v", opts.DeployTimeout, opts.DrainTimeout)
	}
}

func TestNoConfiguredHealthcheckSkipsHealthWait(t *testing.T) {
	env := newEnv(t)

	desired := desiredState("abc123", 1, false)
	desired.Containers[0].Healthcheck = nil
	// The fake checker reports Unhealthy for unknown containers, so a
	// health wait would fail the deploy.
	if err := env.engine.Apply(context.Background(), desired); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if targets := env.proxy.Targets(); len(targets) != 1 {
		t.Fatalf("expected target routed without a configured healthcheck, got %+v", targets)
	}
}

func TestHealthCheckReceivesContainerAddress(t *testing.T) {
	env := newEnv(t)
	env.rt.SetDefaultIPAddress("172.28.0.5")

	checker := &addressRecordingChecker{}
	engine := cutover.New(cutover.Deps{
		Runtime:  env.rt,
		Starter:  reconciler.New(env.rt),
		Health:   checker,
		Proxy:    env.proxy,
		LastGood: env.store,
		Sleeper:  noopSleeper{},
	})

	if err := engine.Apply(context.Background(), desiredState("abc123", 1, false)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if checker.lastAddress != "http://172.28.0.5:3000" {
		t.Fatalf("health check address = %q, want http://172.28.0.5:3000", checker.lastAddress)
	}
}

func TestHealthCheckRefreshesContainerAddressBetweenAttempts(t *testing.T) {
	baseRuntime := fakeruntime.NewRuntime()
	baseRuntime.SetDefaultIPAddress("172.28.0.9")
	rt := &addressBecomesAvailableRuntime{Runtime: baseRuntime}
	checker := &addressAvailabilityChecker{}
	proxyManager := fakeproxy.NewManager()
	engine := cutover.New(cutover.Deps{
		Runtime: rt, Starter: reconciler.New(rt), Health: checker,
		Proxy: proxyManager, Sleeper: noopSleeper{},
	})

	if err := engine.Apply(context.Background(), desiredState("abc123", 1, false)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if checker.checks != 2 {
		t.Fatalf("health checks = %d, want 2", checker.checks)
	}
}

type addressBecomesAvailableRuntime struct {
	runtime.Runtime
	suppressed bool
}

func (r *addressBecomesAvailableRuntime) ListContainers(ctx context.Context, filters runtime.ContainerFilters) ([]runtime.ContainerState, error) {
	states, err := r.Runtime.ListContainers(ctx, filters)
	if err != nil {
		return nil, err
	}
	if len(states) > 0 && !r.suppressed {
		r.suppressed = true
		for i := range states {
			states[i].IPAddress = ""
		}
	}
	return states, nil
}

type addressAvailabilityChecker struct {
	checks int
}

func (c *addressAvailabilityChecker) Check(ctx context.Context, target health.Target) (health.Status, error) {
	_ = ctx
	c.checks++
	if target.Address == "" {
		return health.Unhealthy, nil
	}
	return health.Healthy, nil
}

func TestHealthCheckTimeoutAppliesToEachAttempt(t *testing.T) {
	env := newEnv(t)
	checker := &deadlineChecker{}
	engine := cutover.New(cutover.Deps{
		Runtime: env.rt, Starter: reconciler.New(env.rt), Health: checker,
		Proxy: env.proxy, LastGood: env.store, Sleeper: noopSleeper{},
	})
	desired := desiredState("abc123", 1, false)
	desired.Containers[0].Healthcheck.Timeout = "5ms"
	desired.Containers[0].Healthcheck.Retries = 2

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	err := engine.Apply(ctx, desired)

	if !errors.Is(err, cutover.ErrCandidateUnhealthy) {
		t.Fatalf("Apply error = %v, want ErrCandidateUnhealthy", err)
	}
	if checker.checks != 2 {
		t.Fatalf("health checks = %d, want 2", checker.checks)
	}
	if !checker.sawDeadline {
		t.Fatalf("configured timeout was not applied to health-check context")
	}
}

type deadlineChecker struct {
	checks      int
	sawDeadline bool
}

func (c *deadlineChecker) Check(ctx context.Context, target health.Target) (health.Status, error) {
	_ = target
	c.checks++
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= 20*time.Millisecond {
		c.sawDeadline = true
	}
	<-ctx.Done()
	return health.Unhealthy, ctx.Err()
}

type addressRecordingChecker struct {
	lastAddress string
}

func (c *addressRecordingChecker) Check(ctx context.Context, target health.Target) (health.Status, error) {
	_ = ctx
	c.lastAddress = target.Address
	return health.Healthy, nil
}

type flakyChecker struct {
	checks       int
	healthyAfter int
}

func (c *flakyChecker) Check(ctx context.Context, target health.Target) (health.Status, error) {
	_ = ctx
	_ = target
	c.checks++
	if c.checks >= c.healthyAfter {
		return health.Healthy, nil
	}
	return health.Unhealthy, nil
}

type noopSleeper struct{}

func (noopSleeper) Sleep(ctx context.Context, duration time.Duration) error {
	_ = ctx
	_ = duration
	return nil
}
