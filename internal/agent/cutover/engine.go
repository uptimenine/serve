package cutover

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/uptimenine/serve/internal/agent/health"
	"github.com/uptimenine/serve/internal/agent/proxy"
	"github.com/uptimenine/serve/internal/agent/reconciler"
	"github.com/uptimenine/serve/internal/planner"
	"github.com/uptimenine/serve/internal/runtime"
)

// ErrCandidateUnhealthy is returned when a candidate version never passes
// health checks. The old version keeps serving and the candidate containers
// are removed.
var ErrCandidateUnhealthy = errors.New("candidate version failed health checks")

const (
	defaultHealthInterval = 2 * time.Second
	defaultHealthRetries  = 10
	stopTimeout           = 10 * time.Second
)

type Starter interface {
	EnsureContainer(ctx context.Context, desired planner.DesiredState, container planner.Container) (reconciler.EnsureResult, error)
}

type LastGoodStore interface {
	SaveLastGood(desired planner.DesiredState) error
}

type Sleeper interface {
	Sleep(ctx context.Context, duration time.Duration) error
}

type Deps struct {
	Runtime  runtime.Runtime
	Starter  Starter
	Health   health.Checker
	Proxy    proxy.Manager
	LastGood LastGoodStore
	Sleeper  Sleeper
}

// Engine performs a blue-green deploy: it boots the candidate version next
// to the running one, gates the traffic switch on candidate health, starts
// dependent roles only after the primary role is healthy, and then retires
// old versions per the retention policy.
type Engine struct {
	deps Deps
}

func New(deps Deps) *Engine {
	if deps.Sleeper == nil {
		deps.Sleeper = realSleeper{}
	}
	return &Engine{deps: deps}
}

func (e *Engine) Apply(ctx context.Context, desired planner.DesiredState) error {
	if desired.Network != "" {
		if err := e.deps.Runtime.CreateNetwork(ctx, runtime.NetworkSpec{Name: desired.Network}); err != nil {
			return err
		}
	}

	existing, err := e.listManaged(ctx, desired)
	if err != nil {
		return err
	}

	primary, dependents := splitRoles(desired.Containers)

	created, err := e.startCandidates(ctx, desired, primary)
	if err != nil {
		return errors.Join(err, e.removeContainers(ctx, desired, created))
	}
	if err := e.waitForHealth(ctx, desired, primary); err != nil {
		cleanupErr := e.removeContainers(ctx, desired, created)
		return errors.Join(err, cleanupErr)
	}

	if err := e.switchTraffic(ctx, desired, primary); err != nil {
		return errors.Join(err, e.removeContainers(ctx, desired, created))
	}

	createdDependents, err := e.startCandidates(ctx, desired, dependents)
	if err != nil {
		return errors.Join(err, e.removeContainers(ctx, desired, createdDependents))
	}

	if err := e.retireOldVersions(ctx, desired, existing); err != nil {
		return err
	}

	if e.deps.LastGood != nil {
		if err := e.deps.LastGood.SaveLastGood(desired); err != nil {
			return fmt.Errorf("save last-good state: %w", err)
		}
	}
	return nil
}

func (e *Engine) listManaged(ctx context.Context, desired planner.DesiredState) ([]runtime.ContainerState, error) {
	return e.deps.Runtime.ListContainers(ctx, runtime.ContainerFilters{Labels: map[string]string{
		"serve.managed":     "true",
		"serve.service":     desired.Service,
		"serve.destination": desired.Destination,
	}})
}

func splitRoles(containers []planner.Container) (primary []planner.Container, dependents []planner.Container) {
	for _, container := range containers {
		if container.Proxy {
			primary = append(primary, container)
		} else {
			dependents = append(dependents, container)
		}
	}
	return primary, dependents
}

func (e *Engine) startCandidates(ctx context.Context, desired planner.DesiredState, containers []planner.Container) ([]planner.Container, error) {
	var created []planner.Container
	for _, container := range containers {
		result, err := e.deps.Starter.EnsureContainer(ctx, desired, container)
		if err != nil {
			return created, err
		}
		if result.Created {
			created = append(created, container)
		}
	}
	return created, nil
}

// waitForHealth polls each candidate with a configured healthcheck until it
// is healthy or its retries are exhausted. Candidates without a healthcheck
// are not gated here; kamal-proxy still health-gates its own cutover.
func (e *Engine) waitForHealth(ctx context.Context, desired planner.DesiredState, containers []planner.Container) error {
	checked := make([]planner.Container, 0, len(containers))
	for _, container := range containers {
		if container.Healthcheck != nil {
			checked = append(checked, container)
		}
	}
	if len(checked) == 0 {
		return nil
	}
	if e.deps.Health == nil {
		return fmt.Errorf("health checker is required to cut over proxied containers")
	}

	for _, container := range checked {
		interval, retries := healthPolicy(container.Healthcheck)
		timeout := healthTimeout(container.Healthcheck)

		healthy := false
		for attempt := 0; attempt < retries; attempt++ {
			addresses, err := e.containerAddresses(ctx, desired)
			if err != nil {
				return err
			}
			target := health.Target{
				ContainerName: container.Name,
				Address:       healthAddress(addresses[container.Name], healthPort(container)),
				Port:          healthPort(container),
				Path:          healthPath(container),
			}
			checkCtx := ctx
			cancel := func() {}
			if timeout > 0 {
				checkCtx, cancel = context.WithTimeout(ctx, timeout)
			}
			status, err := e.deps.Health.Check(checkCtx, target)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					return err
				}
			}
			if status == health.Healthy {
				healthy = true
				break
			}
			if attempt < retries-1 {
				if err := e.deps.Sleeper.Sleep(ctx, interval); err != nil {
					return err
				}
			}
		}
		if !healthy {
			return fmt.Errorf("container %s: %w", container.Name, ErrCandidateUnhealthy)
		}
	}
	return nil
}

func (e *Engine) containerAddresses(ctx context.Context, desired planner.DesiredState) (map[string]string, error) {
	states, err := e.listManaged(ctx, desired)
	if err != nil {
		return nil, err
	}
	addresses := make(map[string]string, len(states))
	for _, state := range states {
		addresses[state.Name] = state.IPAddress
	}
	return addresses, nil
}

func healthAddress(ip string, port int) string {
	if ip == "" || port <= 0 {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", ip, port)
}

func (e *Engine) switchTraffic(ctx context.Context, desired planner.DesiredState, primary []planner.Container) error {
	if len(primary) == 0 || e.deps.Proxy == nil {
		return nil
	}

	byRole := map[string][]proxy.Target{}
	for _, container := range primary {
		byRole[container.Role] = append(byRole[container.Role], proxy.Target{
			Service:       desired.Service,
			Role:          container.Role,
			ContainerName: container.Name,
			Port:          healthPort(container),
			HealthPath:    healthPath(container),
		})
	}

	roles := make([]string, 0, len(byRole))
	for role := range byRole {
		roles = append(roles, role)
	}
	sort.Strings(roles)

	opts := routeOptions(desired.Proxy)
	for _, role := range roles {
		if err := e.deps.Proxy.SetTargets(ctx, desired.Service, role, byRole[role], opts); err != nil {
			return err
		}
	}
	return nil
}

func routeOptions(route planner.ProxyRoute) proxy.RouteOptions {
	opts := proxy.RouteOptions{
		Hosts: append([]string(nil), route.Hosts...),
		TLS:   route.SSL,
	}
	if parsed, err := time.ParseDuration(route.DeployTimeout); err == nil && parsed > 0 {
		opts.DeployTimeout = parsed
	}
	if parsed, err := time.ParseDuration(route.DrainTimeout); err == nil && parsed > 0 {
		opts.DrainTimeout = parsed
	}
	return opts
}

func (e *Engine) removeContainers(ctx context.Context, desired planner.DesiredState, containers []planner.Container) error {
	if len(containers) == 0 {
		return nil
	}
	states, err := e.listManaged(ctx, desired)
	if err != nil {
		return err
	}
	byName := map[string]runtime.ContainerState{}
	for _, state := range states {
		byName[state.Name] = state
	}

	var errs []error
	for _, container := range containers {
		state, ok := byName[container.Name]
		if !ok {
			continue
		}
		if state.Running {
			if err := e.deps.Runtime.StopContainer(ctx, state.ID, stopTimeout); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if err := e.deps.Runtime.RemoveContainer(ctx, state.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// retireOldVersions stops containers from versions other than the desired
// one and prunes retained versions beyond retain_containers (which counts
// the current version).
func (e *Engine) retireOldVersions(ctx context.Context, desired planner.DesiredState, existing []runtime.ContainerState) error {
	byVersion := map[string][]runtime.ContainerState{}
	for _, state := range existing {
		version := state.Labels["serve.version"]
		if version == "" || version == desired.Version {
			continue
		}
		byVersion[version] = append(byVersion[version], state)
	}
	if len(byVersion) == 0 {
		return nil
	}

	versions := make([]string, 0, len(byVersion))
	for version := range byVersion {
		versions = append(versions, version)
	}
	sort.Slice(versions, func(i, j int) bool {
		newestI := newestCreation(byVersion[versions[i]])
		newestJ := newestCreation(byVersion[versions[j]])
		if newestI.Equal(newestJ) {
			return versions[i] > versions[j]
		}
		return newestI.After(newestJ)
	})

	keepOld := desired.RetainContainers - 1
	if keepOld < 0 {
		keepOld = 0
	}

	for index, version := range versions {
		for _, state := range byVersion[version] {
			if state.Running {
				if err := e.deps.Runtime.StopContainer(ctx, state.ID, stopTimeout); err != nil {
					return err
				}
			}
			if index >= keepOld {
				if err := e.deps.Runtime.RemoveContainer(ctx, state.ID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func newestCreation(states []runtime.ContainerState) time.Time {
	var newest time.Time
	for _, state := range states {
		if state.CreatedAt.After(newest) {
			newest = state.CreatedAt
		}
	}
	return newest
}

func healthPolicy(check *planner.Healthcheck) (time.Duration, int) {
	interval := defaultHealthInterval
	retries := defaultHealthRetries
	if check == nil {
		return interval, retries
	}
	if check.Interval != "" {
		if parsed, err := time.ParseDuration(check.Interval); err == nil && parsed > 0 {
			interval = parsed
		}
	}
	if check.Retries > 0 {
		retries = check.Retries
	}
	return interval, retries
}

func healthTimeout(check *planner.Healthcheck) time.Duration {
	if check == nil || check.Timeout == "" {
		return 0
	}
	timeout, err := time.ParseDuration(check.Timeout)
	if err != nil || timeout <= 0 {
		return 0
	}
	return timeout
}

func healthPort(container planner.Container) int {
	if container.Healthcheck != nil && container.Healthcheck.Port > 0 {
		return container.Healthcheck.Port
	}
	if len(container.Ports) > 0 {
		return container.Ports[0].ContainerPort
	}
	return 0
}

func healthPath(container planner.Container) string {
	if container.Healthcheck != nil {
		return container.Healthcheck.Path
	}
	return ""
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
