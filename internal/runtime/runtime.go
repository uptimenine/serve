package runtime

import (
	"context"
	"io"
	"time"
)

type ContainerID string

type Runtime interface {
	PullImage(ctx context.Context, image string) error
	CreateContainer(ctx context.Context, spec ContainerSpec) (ContainerID, error)
	StartContainer(ctx context.Context, id ContainerID) error
	StopContainer(ctx context.Context, id ContainerID, timeout time.Duration) error
	RemoveContainer(ctx context.Context, id ContainerID) error
	InspectContainer(ctx context.Context, id ContainerID) (ContainerState, error)
	ListContainers(ctx context.Context, filters ContainerFilters) ([]ContainerState, error)
	Logs(ctx context.Context, id ContainerID, opts LogOptions) (io.ReadCloser, error)
	Events(ctx context.Context) (<-chan RuntimeEvent, error)
	CreateNetwork(ctx context.Context, spec NetworkSpec) error
	ExecContainer(ctx context.Context, id ContainerID, cmd []string) (string, error)
}

type ContainerSpec struct {
	Name     string
	Image    string
	Command  []string
	Labels   map[string]string
	Env      map[string]string
	EnvFiles []string
	Restart  RestartPolicy
	Ports    []Port
	Network  string
	Aliases  []string
	Volumes  []string
}

type RestartPolicy struct {
	Policy      string
	MaxAttempts int
}

type Port struct {
	Name          string
	ContainerPort int
	HostPort      int
	HostIP        string // empty defaults to 127.0.0.1; the proxy publishes on 0.0.0.0
}

type ContainerState struct {
	ID        ContainerID
	Name      string
	Image     string
	Command   []string
	Labels    map[string]string
	EnvFiles  []string
	Restart   RestartPolicy
	Running   bool
	ExitCode  int
	OOMKilled bool
	Health    HealthStatus
	CreatedAt time.Time
	IPAddress string
}

type HealthStatus string

const (
	HealthUnknown   HealthStatus = "unknown"
	HealthStarting  HealthStatus = "starting"
	HealthHealthy   HealthStatus = "healthy"
	HealthUnhealthy HealthStatus = "unhealthy"
)

type ContainerFilters struct {
	Labels map[string]string
}

type LogOptions struct{}

type RuntimeEventType string

const (
	EventDie     RuntimeEventType = "die"
	EventStop    RuntimeEventType = "stop"
	EventOOM     RuntimeEventType = "oom"
	EventStart   RuntimeEventType = "start"
	EventDestroy RuntimeEventType = "destroy"
)

type RuntimeEvent struct {
	Type        RuntimeEventType
	ContainerID ContainerID
	Name        string
	Labels      map[string]string
	ExitCode    int
	OOMKilled   bool
}

type NetworkSpec struct {
	Name string
}
