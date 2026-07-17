package reconciler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/uptimenine/serve/internal/agent/health"
	"github.com/uptimenine/serve/internal/agent/proxy"
	"github.com/uptimenine/serve/internal/agent/secrets"
	"github.com/uptimenine/serve/internal/planner"
	"github.com/uptimenine/serve/internal/runtime"
)

const stopTimeout = 10 * time.Second

type Reconciler struct {
	runtime       runtime.Runtime
	secretStore   secrets.Store
	envFileWriter *secrets.EnvFileWriter
	healthChecker health.Checker
	proxyManager  proxy.Manager
}

type Summary struct {
	NetworksCreated   int
	ContainersCreated int
	ContainersStarted int
	ContainersRemoved int
}

func New(rt runtime.Runtime) *Reconciler {
	return &Reconciler{runtime: rt}
}

func NewWithSecrets(rt runtime.Runtime, store secrets.Store, writer *secrets.EnvFileWriter) *Reconciler {
	return &Reconciler{runtime: rt, secretStore: store, envFileWriter: writer}
}

func NewWithHealth(rt runtime.Runtime, checker health.Checker, manager proxy.Manager) *Reconciler {
	return &Reconciler{runtime: rt, healthChecker: checker, proxyManager: manager}
}

func (r *Reconciler) Reconcile(ctx context.Context, desired planner.DesiredState) (Summary, error) {
	var summary Summary

	if desired.Network != "" {
		if err := r.runtime.CreateNetwork(ctx, runtime.NetworkSpec{Name: desired.Network}); err != nil {
			return summary, err
		}
		summary.NetworksCreated++
	}

	existing, err := r.runtime.ListContainers(ctx, runtime.ContainerFilters{Labels: map[string]string{
		"serve.managed":     "true",
		"serve.service":     desired.Service,
		"serve.destination": desired.Destination,
	}})
	if err != nil {
		return summary, err
	}

	existingByName := map[string]runtime.ContainerState{}
	for _, container := range existing {
		existingByName[container.Name] = container
	}

	desiredNames := map[string]struct{}{}
	for _, container := range desired.Containers {
		desiredNames[container.Name] = struct{}{}
		result, err := r.ensureContainer(ctx, desired, container, existingByName[container.Name])
		if err != nil {
			return summary, err
		}
		if result.RemovedExisting {
			summary.ContainersRemoved++
		}
		if result.Created {
			summary.ContainersCreated++
			summary.ContainersStarted++
		}
		if err := r.reconcileProxyTarget(ctx, desired.Service, container); err != nil {
			return summary, err
		}
	}

	if desired.RetainContainers <= 0 {
		for _, container := range existing {
			if _, stillDesired := desiredNames[container.Name]; stillDesired {
				continue
			}
			if err := r.stopAndRemove(ctx, container); err != nil {
				return summary, err
			}
			summary.ContainersRemoved++
		}
	}

	return summary, nil
}

type EnsureResult struct {
	Created         bool
	RemovedExisting bool
}

// EnsureContainer creates and starts one desired container if no matching
// running container exists, replacing a stale container with the same name
// and re-materializing its secrets. It never touches the proxy.
func (r *Reconciler) EnsureContainer(ctx context.Context, desired planner.DesiredState, container planner.Container) (EnsureResult, error) {
	existing, err := r.runtime.ListContainers(ctx, runtime.ContainerFilters{Labels: map[string]string{
		"serve.managed":     "true",
		"serve.service":     desired.Service,
		"serve.destination": desired.Destination,
	}})
	if err != nil {
		return EnsureResult{}, err
	}
	var current runtime.ContainerState
	for _, state := range existing {
		if state.Name == container.Name {
			current = state
			break
		}
	}
	return r.ensureContainer(ctx, desired, container, current)
}

func (r *Reconciler) ensureContainer(ctx context.Context, desired planner.DesiredState, container planner.Container, current runtime.ContainerState) (EnsureResult, error) {
	var result EnsureResult
	if current.ID != "" && matchingRunningContainer(current, container) {
		return result, nil
	}
	if current.ID != "" {
		if err := r.stopAndRemove(ctx, current); err != nil {
			return result, err
		}
		result.RemovedExisting = true
	}

	// Pull failures are tolerated: the create below succeeds when the image
	// is already present locally and fails with a clear error otherwise.
	_ = r.runtime.PullImage(ctx, container.Image)

	envFiles, err := r.materializeSecrets(ctx, desired, container)
	if err != nil {
		return result, err
	}

	id, createErr := r.runtime.CreateContainer(ctx, runtime.ContainerSpec{
		Name:     container.Name,
		Image:    container.Image,
		Command:  append([]string(nil), container.Command...),
		Labels:   copyStringMap(container.Labels),
		Env:      copyStringMap(container.Env),
		EnvFiles: envFiles,
		Restart:  dockerRestartPolicy(container.Restart),
		Ports:    convertPorts(container.Ports),
		Network:  desired.Network,
		Aliases:  append([]string(nil), container.Aliases...),
		Volumes:  append([]string(nil), container.Volumes...),
	})
	cleanupErr := cleanupEnvFiles(envFiles)
	if createErr != nil {
		return result, errors.Join(createErr, cleanupErr)
	}
	if cleanupErr != nil {
		return result, errors.Join(cleanupErr, r.runtime.RemoveContainer(ctx, id))
	}
	if err := r.runtime.StartContainer(ctx, id); err != nil {
		return result, errors.Join(err, r.runtime.RemoveContainer(ctx, id))
	}
	result.Created = true
	return result, nil
}

func cleanupEnvFiles(paths []string) error {
	var errs []error
	for _, path := range paths {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove env file %q: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

func (r *Reconciler) stopAndRemove(ctx context.Context, container runtime.ContainerState) error {
	if container.Running {
		if err := r.runtime.StopContainer(ctx, container.ID, stopTimeout); err != nil {
			return err
		}
	}
	return r.runtime.RemoveContainer(ctx, container.ID)
}

func (r *Reconciler) materializeSecrets(ctx context.Context, desired planner.DesiredState, container planner.Container) ([]string, error) {
	if len(container.SecretNames) == 0 {
		return nil, nil
	}
	if r.secretStore == nil {
		return nil, fmt.Errorf("secret store is required for container %s", container.Name)
	}
	if r.envFileWriter == nil {
		return nil, fmt.Errorf("env file writer is required for container %s", container.Name)
	}

	resolved, err := r.secretStore.Resolve(ctx, secrets.Request{
		Ref:           container.SecretsRef,
		Names:         container.SecretNames,
		Ciphertext:    container.SecretCiphertext,
		EncryptedFile: desired.SecretsFile,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve secrets for %s: %w", container.Name, err)
	}
	envFile, err := r.envFileWriter.Write(desired.Service, container.Role, resolved)
	if err != nil {
		return nil, fmt.Errorf("write secrets env file for %s: %w", container.Name, err)
	}
	return []string{envFile}, nil
}

func (r *Reconciler) reconcileProxyTarget(ctx context.Context, service string, container planner.Container) error {
	if !container.Proxy {
		return nil
	}
	if r.healthChecker == nil || r.proxyManager == nil {
		return nil
	}

	target := health.Target{
		ContainerName: container.Name,
		Port:          targetPort(container),
		Path:          targetHealthPath(container),
	}
	status, err := r.healthChecker.Check(ctx, target)
	if err != nil {
		return err
	}

	proxyTarget := proxy.Target{
		Service:       service,
		Role:          container.Role,
		ContainerName: container.Name,
		Port:          target.Port,
		HealthPath:    target.Path,
	}
	if status == health.Healthy {
		return r.proxyManager.AddTarget(ctx, proxyTarget)
	}
	return r.proxyManager.RemoveTarget(ctx, proxyTarget)
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

func matchingRunningContainer(current runtime.ContainerState, desired planner.Container) bool {
	return current.Running &&
		current.Name == desired.Name &&
		current.Image == desired.Image &&
		labelsInclude(current.Labels, desired.Labels)
}

func labelsInclude(current map[string]string, desired map[string]string) bool {
	for key, value := range desired {
		if current[key] != value {
			return false
		}
	}
	return true
}

func dockerRestartPolicy(restart planner.Restart) runtime.RestartPolicy {
	if restart.Controller != "docker" {
		return runtime.RestartPolicy{}
	}
	policy := restart.Policy
	if policy == "" {
		policy = "always"
	}
	return runtime.RestartPolicy{Policy: policy, MaxAttempts: restart.MaxAttempts}
}

func convertPorts(ports []planner.Port) []runtime.Port {
	if len(ports) == 0 {
		return nil
	}
	converted := make([]runtime.Port, 0, len(ports))
	for _, port := range ports {
		converted = append(converted, runtime.Port{Name: port.Name, ContainerPort: port.ContainerPort})
	}
	return converted
}

func copyStringMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	copy := make(map[string]string, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}
