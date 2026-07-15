package fake

import (
	"context"

	"github.com/uptimenine/serve/internal/agent/health"
)

type Checker struct {
	statuses map[string]health.Status
	checks   int
}

func NewChecker() *Checker {
	return &Checker{statuses: map[string]health.Status{}}
}

func (c *Checker) SetStatus(containerName string, status health.Status) {
	c.statuses[containerName] = status
}

func (c *Checker) Check(ctx context.Context, target health.Target) (health.Status, error) {
	_ = ctx
	c.checks++
	status, ok := c.statuses[target.ContainerName]
	if !ok {
		return health.Unhealthy, nil
	}
	return status, nil
}

func (c *Checker) CheckCount() int {
	return c.checks
}
