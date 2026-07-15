package fake

import (
	"context"
	"sort"

	"github.com/uptimenine/serve/internal/agent/proxy"
)

type Manager struct {
	targets     map[string]proxy.Target
	removeCount int
	setCalls    int
	lastRoute   proxy.RouteOptions
}

func NewManager() *Manager {
	return &Manager{targets: map[string]proxy.Target{}}
}

func (m *Manager) AddTarget(ctx context.Context, target proxy.Target) error {
	_ = ctx
	m.targets[target.ContainerName] = target
	return nil
}

func (m *Manager) RemoveTarget(ctx context.Context, target proxy.Target) error {
	_ = ctx
	delete(m.targets, target.ContainerName)
	m.removeCount++
	return nil
}

func (m *Manager) SetTargets(ctx context.Context, service string, role string, targets []proxy.Target, opts proxy.RouteOptions) error {
	_ = ctx
	for name, existing := range m.targets {
		if existing.Service == service && existing.Role == role {
			delete(m.targets, name)
		}
	}
	for _, target := range targets {
		m.targets[target.ContainerName] = target
	}
	m.setCalls++
	m.lastRoute = opts
	return nil
}

func (m *Manager) SetTargetsCalls() int {
	return m.setCalls
}

func (m *Manager) LastRouteOptions() proxy.RouteOptions {
	return m.lastRoute
}

func (m *Manager) Targets() []proxy.Target {
	keys := make([]string, 0, len(m.targets))
	for key := range m.targets {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	targets := make([]proxy.Target, 0, len(keys))
	for _, key := range keys {
		targets = append(targets, m.targets[key])
	}
	return targets
}

func (m *Manager) RemoveCount() int {
	return m.removeCount
}
