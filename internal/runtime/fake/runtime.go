package fake

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/uptimenine/serve/internal/runtime"
)

type Runtime struct {
	mu          sync.Mutex
	nextID      int
	containers  map[runtime.ContainerID]runtime.ContainerState
	specs       map[string]runtime.ContainerSpec
	logs        map[runtime.ContainerID]string
	subscribers []chan runtime.RuntimeEvent
	networks    map[string]runtime.NetworkSpec
	operations  []string
	execResults map[string]execResult
	baseTime    time.Time
	defaultIP   string
	pullErr     error
}

type execResult struct {
	output string
	err    error
}

func NewRuntime() *Runtime {
	return &Runtime{
		containers:  map[runtime.ContainerID]runtime.ContainerState{},
		specs:       map[string]runtime.ContainerSpec{},
		logs:        map[runtime.ContainerID]string{},
		networks:    map[string]runtime.NetworkSpec{},
		execResults: map[string]execResult{},
		baseTime:    time.Now(),
	}
}

func (r *Runtime) PullImage(ctx context.Context, image string) error {
	_ = ctx

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.pullErr != nil {
		return r.pullErr
	}
	r.operations = append(r.operations, "pull_image:"+image)
	return nil
}

func (r *Runtime) SetPullError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.pullErr = err
}

func (r *Runtime) CreateContainer(ctx context.Context, spec runtime.ContainerSpec) (runtime.ContainerID, error) {
	_ = ctx

	r.mu.Lock()
	defer r.mu.Unlock()

	r.nextID++
	id := runtime.ContainerID(fmt.Sprintf("fake-%d", r.nextID))
	r.operations = append(r.operations, "create_container:"+spec.Name)
	r.specs[spec.Name] = spec
	r.containers[id] = runtime.ContainerState{
		ID:       id,
		Name:     spec.Name,
		Image:    spec.Image,
		Command:  append([]string(nil), spec.Command...),
		Labels:   copyStringMap(spec.Labels),
		EnvFiles: append([]string(nil), spec.EnvFiles...),
		Health:   runtime.HealthUnknown,
		// Spread creation times so ordering by CreatedAt is deterministic
		// even when a test creates several containers back to back.
		CreatedAt: r.baseTime.Add(time.Duration(r.nextID) * time.Second),
		IPAddress: r.defaultIP,
	}
	return id, nil
}

func (r *Runtime) StartContainer(ctx context.Context, id runtime.ContainerID) error {
	_ = ctx

	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.containers[id]
	if !ok {
		return fmt.Errorf("container not found: %s", id)
	}
	state.Running = true
	state.ExitCode = 0
	state.OOMKilled = false
	r.operations = append(r.operations, "start_container:"+string(id))
	r.containers[id] = state
	return nil
}

func (r *Runtime) StopContainer(ctx context.Context, id runtime.ContainerID, timeout time.Duration) error {
	_ = ctx
	_ = timeout

	r.mu.Lock()
	state, ok := r.containers[id]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("container not found: %s", id)
	}
	state.Running = false
	r.operations = append(r.operations, "stop_container:"+string(id))
	r.containers[id] = state
	event := runtime.RuntimeEvent{Type: runtime.EventStop, ContainerID: id, Name: state.Name, Labels: copyStringMap(state.Labels)}
	r.mu.Unlock()

	r.publish(event)
	return nil
}

func (r *Runtime) RemoveContainer(ctx context.Context, id runtime.ContainerID) error {
	_ = ctx

	r.mu.Lock()
	state, ok := r.containers[id]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("container not found: %s", id)
	}
	r.operations = append(r.operations, "remove_container:"+state.Name)
	delete(r.containers, id)
	event := runtime.RuntimeEvent{Type: runtime.EventDestroy, ContainerID: id, Name: state.Name, Labels: copyStringMap(state.Labels)}
	r.mu.Unlock()

	r.publish(event)
	return nil
}

func (r *Runtime) InspectContainer(ctx context.Context, id runtime.ContainerID) (runtime.ContainerState, error) {
	_ = ctx

	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.containers[id]
	if !ok {
		return runtime.ContainerState{}, fmt.Errorf("container not found: %s", id)
	}
	return copyContainerState(state), nil
}

func (r *Runtime) ListContainers(ctx context.Context, filters runtime.ContainerFilters) ([]runtime.ContainerState, error) {
	_ = ctx

	r.mu.Lock()
	defer r.mu.Unlock()

	ids := make([]string, 0, len(r.containers))
	byID := map[string]runtime.ContainerID{}
	for id := range r.containers {
		key := string(id)
		ids = append(ids, key)
		byID[key] = id
	}
	sort.Strings(ids)

	var states []runtime.ContainerState
	for _, key := range ids {
		state := r.containers[byID[key]]
		if !matchesLabels(state.Labels, filters.Labels) {
			continue
		}
		states = append(states, copyContainerState(state))
	}
	return states, nil
}

func (r *Runtime) Logs(ctx context.Context, id runtime.ContainerID, opts runtime.LogOptions) (io.ReadCloser, error) {
	_ = ctx
	_ = opts

	r.mu.Lock()
	_, ok := r.containers[id]
	logs := r.logs[id]
	r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("container not found: %s", id)
	}
	return io.NopCloser(strings.NewReader(logs)), nil
}

func (r *Runtime) SetLogs(id runtime.ContainerID, logs string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.logs[id] = logs
}

func (r *Runtime) Events(ctx context.Context) (<-chan runtime.RuntimeEvent, error) {
	ch := make(chan runtime.RuntimeEvent, 16)

	r.mu.Lock()
	r.subscribers = append(r.subscribers, ch)
	r.mu.Unlock()

	go func() {
		<-ctx.Done()
		r.mu.Lock()
		r.unsubscribeLocked(ch)
		close(ch)
		r.mu.Unlock()
	}()

	return ch, nil
}

func (r *Runtime) WaitForSubscribers(t interface{ Fatalf(string, ...any) }, count int) {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		actual := len(r.subscribers)
		r.mu.Unlock()
		if actual >= count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d runtime event subscriber(s)", count)
}

func (r *Runtime) CreateNetwork(ctx context.Context, spec runtime.NetworkSpec) error {
	_ = ctx

	r.mu.Lock()
	defer r.mu.Unlock()

	r.operations = append(r.operations, "create_network:"+spec.Name)
	r.networks[spec.Name] = spec
	return nil
}

func (r *Runtime) ExecContainer(ctx context.Context, id runtime.ContainerID, cmd []string) (string, error) {
	_ = ctx

	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.containers[id]
	if !ok {
		return "", fmt.Errorf("container not found: %s", id)
	}
	if !state.Running {
		return "", fmt.Errorf("container not running: %s", state.Name)
	}
	r.operations = append(r.operations, "exec_container:"+state.Name+":"+strings.Join(cmd, " "))
	result := r.execResults[state.Name]
	return result.output, result.err
}

func (r *Runtime) SetDefaultIPAddress(ip string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.defaultIP = ip
}

func (r *Runtime) SetExecResult(containerName string, output string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.execResults[containerName] = execResult{output: output, err: err}
}

func (r *Runtime) CreatedSpec(name string) (runtime.ContainerSpec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	spec, ok := r.specs[name]
	return spec, ok
}

func (r *Runtime) Operations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.operations...)
}

func (r *Runtime) ClearOperations() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.operations = nil
}

func (r *Runtime) Die(id runtime.ContainerID, exitCode int, oomKilled bool) {
	r.mu.Lock()
	state, ok := r.containers[id]
	if !ok {
		r.mu.Unlock()
		return
	}
	state.Running = false
	state.ExitCode = exitCode
	state.OOMKilled = oomKilled
	r.containers[id] = state
	event := runtime.RuntimeEvent{
		Type:        runtime.EventDie,
		ContainerID: id,
		Name:        state.Name,
		Labels:      copyStringMap(state.Labels),
		ExitCode:    exitCode,
		OOMKilled:   oomKilled,
	}
	r.mu.Unlock()

	r.publish(event)
}

func (r *Runtime) publish(event runtime.RuntimeEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, subscriber := range r.subscribers {
		select {
		case subscriber <- event:
		default:
		}
	}
}

func (r *Runtime) unsubscribeLocked(ch chan runtime.RuntimeEvent) {
	for i, subscriber := range r.subscribers {
		if subscriber == ch {
			r.subscribers = append(r.subscribers[:i], r.subscribers[i+1:]...)
			return
		}
	}
}

func matchesLabels(containerLabels map[string]string, filters map[string]string) bool {
	for key, value := range filters {
		if containerLabels[key] != value {
			return false
		}
	}
	return true
}

func copyContainerState(state runtime.ContainerState) runtime.ContainerState {
	state.Command = append([]string(nil), state.Command...)
	state.Labels = copyStringMap(state.Labels)
	state.EnvFiles = append([]string(nil), state.EnvFiles...)
	return state
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
