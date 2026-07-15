package proxy

import (
	"context"
	"time"
)

type Target struct {
	Service       string
	Role          string
	ContainerName string
	Port          int
	HealthPath    string
}

// RouteOptions carries per-route proxy settings from serve.yml: public
// hostnames, TLS, and cutover timeouts. Zero values fall back to provider
// defaults.
type RouteOptions struct {
	Hosts         []string
	TLS           bool
	DeployTimeout time.Duration
	DrainTimeout  time.Duration
}

type Manager interface {
	AddTarget(ctx context.Context, target Target) error
	RemoveTarget(ctx context.Context, target Target) error
	// SetTargets atomically replaces the routed target set for a
	// service role. An empty set unroutes the role entirely.
	SetTargets(ctx context.Context, service string, role string, targets []Target, opts RouteOptions) error
}
