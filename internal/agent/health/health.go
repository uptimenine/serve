package health

import (
	"context"
	"net/http"
	"strings"
)

type Status string

const (
	Healthy   Status = "healthy"
	Unhealthy Status = "unhealthy"
)

type Target struct {
	ContainerName string
	Address       string
	Port          int
	Path          string
}

type Checker interface {
	Check(ctx context.Context, target Target) (Status, error)
}

type HTTPChecker struct {
	client *http.Client
}

func NewHTTPChecker(client *http.Client) *HTTPChecker {
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPChecker{client: client}
}

func (c *HTTPChecker) Check(ctx context.Context, target Target) (Status, error) {
	url := target.Address
	if strings.TrimSpace(url) == "" {
		return Unhealthy, nil
	}
	path := target.Path
	if path == "" {
		path = "/"
	}
	url = strings.TrimRight(url, "/") + "/" + strings.TrimLeft(path, "/")

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Unhealthy, err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return Unhealthy, nil
	}
	defer response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 400 {
		return Healthy, nil
	}
	return Unhealthy, nil
}
