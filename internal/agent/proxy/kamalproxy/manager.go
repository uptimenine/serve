package kamalproxy

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/uptimenine/serve/internal/agent/proxy"
	"github.com/uptimenine/serve/internal/runtime"
)

const (
	DefaultImage         = "basecamp/kamal-proxy:v0.8.4"
	DefaultContainerName = "serve-proxy"
	DefaultDeployTimeout = 30 * time.Second
	DefaultDrainTimeout  = 30 * time.Second
)

type Options struct {
	Image         string
	ContainerName string
	Network       string
	DeployTimeout time.Duration
	DrainTimeout  time.Duration
}

// Manager implements proxy.Manager by driving a kamal-proxy container
// through docker exec. It boots the proxy container on first use (or adopts
// a running one). kamal-proxy routes a set of targets per service, so the
// manager tracks the full set per service role and deploys it atomically,
// skipping exec calls when the set is unchanged.
type Manager struct {
	runtime runtime.Runtime
	opts    Options

	mu sync.Mutex
	// routed maps kamal-proxy service name to its target set. A present
	// key with an empty routedService means the service is known-removed.
	routed  map[string]routedService
	proxyID runtime.ContainerID
}

type routedService struct {
	addresses  []string // sorted
	healthPath string
	route      proxy.RouteOptions
}

func New(rt runtime.Runtime, opts Options) *Manager {
	if opts.Image == "" {
		opts.Image = DefaultImage
	}
	if opts.ContainerName == "" {
		opts.ContainerName = DefaultContainerName
	}
	if opts.DeployTimeout == 0 {
		opts.DeployTimeout = DefaultDeployTimeout
	}
	if opts.DrainTimeout == 0 {
		opts.DrainTimeout = DefaultDrainTimeout
	}
	return &Manager{runtime: rt, opts: opts, routed: map[string]routedService{}}
}

func (m *Manager) AddTarget(ctx context.Context, target proxy.Target) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	service := serviceKey(target.Service, target.Role)
	current, known := m.routed[service]
	next := routedService{
		addresses:  addAddress(current.addresses, address(target)),
		healthPath: firstNonEmpty(target.HealthPath, current.healthPath),
		route:      current.route,
	}
	return m.apply(ctx, service, current, next, !known)
}

func (m *Manager) RemoveTarget(ctx context.Context, target proxy.Target) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	service := serviceKey(target.Service, target.Role)
	current, known := m.routed[service]
	if !known {
		return nil
	}
	next := routedService{
		addresses:  removeAddress(current.addresses, address(target)),
		healthPath: current.healthPath,
		route:      current.route,
	}
	return m.apply(ctx, service, current, next, false)
}

func (m *Manager) SetTargets(ctx context.Context, service string, role string, targets []proxy.Target, opts proxy.RouteOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := serviceKey(service, role)
	current := m.routed[key]
	opts.Hosts = append([]string(nil), opts.Hosts...)
	sort.Strings(opts.Hosts)
	next := routedService{route: opts}
	for _, target := range targets {
		next.addresses = addAddress(next.addresses, address(target))
		next.healthPath = firstNonEmpty(target.HealthPath, next.healthPath)
	}
	return m.apply(ctx, key, current, next, false)
}

// apply reconciles the routed set for one kamal-proxy service. force issues
// the proxy call even when the set looks unchanged (used when the manager
// has no knowledge of the service yet, e.g. right after an agent restart).
// Callers must hold m.mu.
func (m *Manager) apply(ctx context.Context, service string, current routedService, next routedService, force bool) error {
	previousProxyID := m.proxyID
	proxyID, err := m.ensureProxy(ctx)
	if err != nil {
		return err
	}
	if !force && sameRoutedService(current, next) && previousProxyID != "" && previousProxyID == proxyID {
		return nil
	}

	if len(next.addresses) == 0 {
		if _, err := m.runtime.ExecContainer(ctx, proxyID, []string{"kamal-proxy", "remove", service}); err != nil {
			return fmt.Errorf("kamal-proxy remove %s: %w", service, err)
		}
		m.routed[service] = routedService{healthPath: next.healthPath}
		return nil
	}

	cmd := []string{"kamal-proxy", "deploy", service}
	for _, addr := range next.addresses {
		cmd = append(cmd, "--target="+addr)
	}
	for _, host := range next.route.Hosts {
		cmd = append(cmd, "--host="+host)
	}
	if next.healthPath != "" {
		cmd = append(cmd, "--health-check-path="+next.healthPath)
	}
	if next.route.TLS {
		cmd = append(cmd, "--tls")
	}
	deployTimeout := m.opts.DeployTimeout
	if next.route.DeployTimeout > 0 {
		deployTimeout = next.route.DeployTimeout
	}
	drainTimeout := m.opts.DrainTimeout
	if next.route.DrainTimeout > 0 {
		drainTimeout = next.route.DrainTimeout
	}
	cmd = append(cmd,
		"--deploy-timeout="+formatTimeout(deployTimeout),
		"--drain-timeout="+formatTimeout(drainTimeout),
	)
	if _, err := m.runtime.ExecContainer(ctx, proxyID, cmd); err != nil {
		return fmt.Errorf("kamal-proxy deploy %s: %w", service, err)
	}
	m.routed[service] = next
	return nil
}

func sameRoutedService(a routedService, b routedService) bool {
	if !sameAddresses(a.addresses, b.addresses) || a.healthPath != b.healthPath {
		return false
	}
	if a.route.TLS != b.route.TLS || a.route.DeployTimeout != b.route.DeployTimeout || a.route.DrainTimeout != b.route.DrainTimeout {
		return false
	}
	return sameAddresses(a.route.Hosts, b.route.Hosts)
}

// ensureProxy adopts a running proxy container or boots a new one.
// Callers must hold m.mu.
func (m *Manager) ensureProxy(ctx context.Context) (runtime.ContainerID, error) {
	if m.proxyID != "" {
		state, err := m.runtime.InspectContainer(ctx, m.proxyID)
		if err == nil && state.Running {
			return m.proxyID, nil
		}
		m.proxyID = ""
	}

	states, err := m.runtime.ListContainers(ctx, runtime.ContainerFilters{Labels: map[string]string{
		"serve.managed":        "true",
		"serve.container_type": "proxy",
	}})
	if err != nil {
		return "", err
	}
	for _, state := range states {
		if state.Name != m.opts.ContainerName {
			continue
		}
		if state.Running {
			m.proxyID = state.ID
			return state.ID, nil
		}
		if err := m.runtime.RemoveContainer(ctx, state.ID); err != nil {
			return "", err
		}
	}

	if err := m.runtime.PullImage(ctx, m.opts.Image); err != nil {
		return "", fmt.Errorf("pull proxy image %s: %w", m.opts.Image, err)
	}
	id, err := m.runtime.CreateContainer(ctx, runtime.ContainerSpec{
		Name:  m.opts.ContainerName,
		Image: m.opts.Image,
		Labels: map[string]string{
			"serve.managed":        "true",
			"serve.container_type": "proxy",
		},
		Ports: []runtime.Port{
			{Name: "http", ContainerPort: 80, HostPort: 80, HostIP: "0.0.0.0"},
			{Name: "https", ContainerPort: 443, HostPort: 443, HostIP: "0.0.0.0"},
		},
		Network: m.opts.Network,
		Restart: runtime.RestartPolicy{Policy: "unless-stopped"},
		Volumes: []string{"serve-proxy-data:/home/kamal-proxy/.config/kamal-proxy"},
	})
	if err != nil {
		return "", fmt.Errorf("create proxy container: %w", err)
	}
	if err := m.runtime.StartContainer(ctx, id); err != nil {
		return "", fmt.Errorf("start proxy container: %w", err)
	}
	m.proxyID = id
	return id, nil
}

func serviceKey(service string, role string) string {
	return service + "-" + role
}

func address(target proxy.Target) string {
	return fmt.Sprintf("%s:%d", target.ContainerName, target.Port)
}

func addAddress(addresses []string, addr string) []string {
	for _, existing := range addresses {
		if existing == addr {
			return append([]string(nil), addresses...)
		}
	}
	next := append(append([]string(nil), addresses...), addr)
	sort.Strings(next)
	return next
}

func removeAddress(addresses []string, addr string) []string {
	var next []string
	for _, existing := range addresses {
		if existing != addr {
			next = append(next, existing)
		}
	}
	return next
}

func sameAddresses(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func formatTimeout(d time.Duration) string {
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
