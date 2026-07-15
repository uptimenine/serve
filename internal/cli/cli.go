package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/uptimenine/serve/internal/agent/cutover"
	"github.com/uptimenine/serve/internal/agent/daemon"
	"github.com/uptimenine/serve/internal/agent/events"
	"github.com/uptimenine/serve/internal/agent/healing"
	"github.com/uptimenine/serve/internal/agent/health"
	"github.com/uptimenine/serve/internal/agent/proxy/kamalproxy"
	"github.com/uptimenine/serve/internal/agent/reconciler"
	"github.com/uptimenine/serve/internal/agent/secrets"
	"github.com/uptimenine/serve/internal/agent/secrets/sops"
	agentstate "github.com/uptimenine/serve/internal/agent/state"
	"github.com/uptimenine/serve/internal/config"
	"github.com/uptimenine/serve/internal/planner"
	"github.com/uptimenine/serve/internal/runtime"
	dockerruntime "github.com/uptimenine/serve/internal/runtime/docker"
)

// secretEnvFileDir keeps decrypted environment files on the host's tmpfs.
const secretEnvFileDir = "/run/serve/env"

type Command struct {
	version        string
	runtimeFactory func() (runtime.Runtime, error)
	runner         Runner
	sshRunner      SSHRunner
}

type Runner interface {
	Run(ctx context.Context, name string, args ...string) error
}

// SSHRunner executes one command on a remote host, feeding it stdin and
// streaming its stdout.
type SSHRunner interface {
	Run(ctx context.Context, host string, command string, stdin io.Reader, stdout io.Writer) error
}

type Option func(*Command)

func New(version string, opts ...Option) *Command {
	cmd := &Command{
		version: version,
		runtimeFactory: func() (runtime.Runtime, error) {
			return dockerruntime.NewFromEnv()
		},
		runner:    execRunner{},
		sshRunner: execSSHRunner{},
	}
	for _, opt := range opts {
		opt(cmd)
	}
	return cmd
}

func WithRuntime(rt runtime.Runtime) Option {
	return func(c *Command) {
		c.runtimeFactory = func() (runtime.Runtime, error) {
			return rt, nil
		}
	}
}

func WithRunner(runner Runner) Option {
	return func(c *Command) {
		c.runner = runner
	}
}

func WithSSHRunner(runner SSHRunner) Option {
	return func(c *Command) {
		c.sshRunner = runner
	}
}

func (c *Command) Run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		printHelp(stderr)
		return 1
	}

	var err error
	switch args[0] {
	case "help", "--help", "-h":
		printHelp(stdout)
		return 0
	case "version":
		fmt.Fprintln(stdout, c.version)
		return 0
	case "init":
		err = c.runInit(args[1:], stdout)
	case "status":
		err = c.runStatus(ctx, args[1:], stdout)
	case "agent":
		err = c.runAgent(ctx, args[1:], stdout)
	case "deploy":
		err = c.runDeploy(ctx, args[1:], stdout)
	case "logs":
		err = c.runLogs(ctx, args[1:], stdout)
	case "events":
		err = c.runEvents(ctx, args[1:], stdout)
	case "doctor":
		err = c.runDoctor(ctx, stdout)
	case "remove":
		err = c.runRemove(ctx, args[1:], stdout)
	case "rollback":
		err = c.runRollback(ctx, args[1:], stdout)
	case "secrets":
		err = c.runSecrets(ctx, args[1:], stdout)
	case "prune":
		err = c.runPrune(ctx, args[1:], stdout)
	case "exec":
		err = c.runExec(ctx, args[1:], stdout)
	case "setup":
		fmt.Fprintf(stderr, "serve %s is not implemented yet\n", args[0])
		return 1
	default:
		fmt.Fprintf(stderr, "unknown command: %s\n\n", args[0])
		printHelp(stderr)
		return 1
	}

	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func (c *Command) runInit(args []string, stdout io.Writer) error {
	path := "serve.yml"
	force := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--path":
			if i+1 >= len(args) {
				return fmt.Errorf("serve init: --path requires a value")
			}
			path = args[i+1]
			i++
		case "--force":
			force = true
		default:
			return fmt.Errorf("serve init: unknown argument %s", args[i])
		}
	}

	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("serve init: %s already exists; use --force to overwrite", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("serve init: inspect %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(starterConfig), 0o644); err != nil {
		return fmt.Errorf("serve init: write %s: %w", path, err)
	}
	fmt.Fprintf(stdout, "Created %s\n", path)
	return nil
}

func (c *Command) runStatus(ctx context.Context, args []string, stdout io.Writer) error {
	configPath := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config":
			if i+1 >= len(args) {
				return fmt.Errorf("serve status: --config requires a value")
			}
			i++
			configPath = args[i]
		default:
			return fmt.Errorf("serve status: unknown argument %s", args[i])
		}
	}
	if configPath != "" {
		return c.remoteStatus(ctx, configPath, stdout)
	}

	rt, err := c.runtimeFactory()
	if err != nil {
		return fmt.Errorf("serve status: create runtime: %w", err)
	}
	containers, err := managedContainers(ctx, rt, map[string]string{})
	if err != nil {
		return fmt.Errorf("serve status: list containers: %w", err)
	}
	if len(containers) == 0 {
		fmt.Fprintln(stdout, "No Serve-managed containers found.")
		return nil
	}
	return printStatus(stdout, containers)
}

// remoteStatus asks each configured host's agent for its status over SSH.
func (c *Command) remoteStatus(ctx context.Context, configPath string, stdout io.Writer) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("serve status: %w", err)
	}
	hosts := configHosts(cfg)
	if len(hosts) == 0 {
		return fmt.Errorf("serve status: no hosts configured")
	}
	command := fmt.Sprintf("sudo serve agent status --json --socket %s", daemon.DefaultSocketPath)
	for _, host := range hosts {
		fmt.Fprintf(stdout, "== %s\n", host)
		if err := c.sshRunner.Run(ctx, host, command, nil, stdout); err != nil {
			return fmt.Errorf("serve status: %s: %w", host, err)
		}
	}
	return nil
}

func (c *Command) runLogs(ctx context.Context, args []string, stdout io.Writer) error {
	host, rest, err := parseHostFlag("serve logs", args)
	if err != nil {
		return err
	}
	if host != "" {
		container := ""
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--container":
				if i+1 >= len(rest) {
					return fmt.Errorf("serve logs: --container requires a value")
				}
				i++
				container = rest[i]
			default:
				return fmt.Errorf("serve logs: unknown argument %s with --host", rest[i])
			}
		}
		if container == "" {
			return fmt.Errorf("serve logs: --container is required with --host")
		}
		command := fmt.Sprintf("sudo serve agent logs --container %s --socket %s", shellQuote(container), daemon.DefaultSocketPath)
		if err := c.sshRunner.Run(ctx, host, command, nil, stdout); err != nil {
			return fmt.Errorf("serve logs: %s: %w", host, err)
		}
		return nil
	}

	options, err := parseContainerSelection("serve logs", rest)
	if err != nil {
		return err
	}
	rt, err := c.runtimeFactory()
	if err != nil {
		return fmt.Errorf("serve logs: create runtime: %w", err)
	}
	container, err := selectContainer(ctx, rt, options)
	if err != nil {
		return fmt.Errorf("serve logs: %w", err)
	}
	logs, err := rt.Logs(ctx, container.ID, runtime.LogOptions{})
	if err != nil {
		return fmt.Errorf("serve logs: stream %s: %w", container.Name, err)
	}
	defer logs.Close()
	_, err = io.Copy(stdout, logs)
	return err
}

func (c *Command) runEvents(ctx context.Context, args []string, stdout io.Writer) error {
	host, rest, err := parseHostFlag("serve events", args)
	if err != nil {
		return err
	}
	once := false
	for _, arg := range rest {
		if arg != "--once" {
			return fmt.Errorf("serve events: unknown argument %s", arg)
		}
		once = true
	}
	if host != "" {
		command := "sudo serve agent events"
		if once {
			command += " --once"
		}
		command += " --socket " + daemon.DefaultSocketPath
		if err := c.sshRunner.Run(ctx, host, command, nil, stdout); err != nil {
			return fmt.Errorf("serve events: %s: %w", host, err)
		}
		return nil
	}
	rt, err := c.runtimeFactory()
	if err != nil {
		return fmt.Errorf("serve events: create runtime: %w", err)
	}
	events, err := rt.Events(ctx)
	if err != nil {
		return fmt.Errorf("serve events: subscribe: %w", err)
	}
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return nil
			}
			fmt.Fprintf(stdout, "%s container=%s exit_code=%d oom=%t\n", event.Type, event.Name, event.ExitCode, event.OOMKilled)
			if once {
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// runExec runs a command inside a managed container, locally through the
// runtime or on a remote host through its serve binary.
func (c *Command) runExec(ctx context.Context, args []string, stdout io.Writer) error {
	host := ""
	container := ""
	var command []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--host":
			if i+1 >= len(args) {
				return fmt.Errorf("serve exec: --host requires a value")
			}
			i++
			host = args[i]
		case "--container":
			if i+1 >= len(args) {
				return fmt.Errorf("serve exec: --container requires a value")
			}
			i++
			container = args[i]
		case "--":
			command = args[i+1:]
			i = len(args)
		default:
			return fmt.Errorf("serve exec: unknown argument %s", args[i])
		}
	}
	if container == "" {
		return fmt.Errorf("serve exec: --container is required")
	}
	if len(command) == 0 {
		return fmt.Errorf("serve exec: command is required after --")
	}

	if host != "" {
		quotedCommand := make([]string, len(command))
		for i, arg := range command {
			quotedCommand[i] = shellQuote(arg)
		}
		remote := fmt.Sprintf("sudo serve exec --container %s -- %s", shellQuote(container), strings.Join(quotedCommand, " "))
		if err := c.sshRunner.Run(ctx, host, remote, nil, stdout); err != nil {
			return fmt.Errorf("serve exec: %s: %w", host, err)
		}
		return nil
	}

	rt, err := c.runtimeFactory()
	if err != nil {
		return fmt.Errorf("serve exec: create runtime: %w", err)
	}
	containers, err := managedContainers(ctx, rt, map[string]string{})
	if err != nil {
		return fmt.Errorf("serve exec: list containers: %w", err)
	}
	var id runtime.ContainerID
	for _, state := range containers {
		if state.Name == container {
			id = state.ID
			break
		}
	}
	if id == "" {
		return fmt.Errorf("serve exec: container %s not found", container)
	}
	output, err := rt.ExecContainer(ctx, id, command)
	if output != "" {
		fmt.Fprint(stdout, output)
	}
	if err != nil {
		return fmt.Errorf("serve exec: %w", err)
	}
	return nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (c *Command) runDoctor(ctx context.Context, stdout io.Writer) error {
	rt, err := c.runtimeFactory()
	if err != nil {
		return fmt.Errorf("serve doctor: create runtime: %w", err)
	}
	if _, err := rt.ListContainers(ctx, runtime.ContainerFilters{}); err != nil {
		return fmt.Errorf("serve doctor: Docker reachable: failed: %w", err)
	}
	fmt.Fprintln(stdout, "Docker reachable: ok")
	if err := rt.CreateNetwork(ctx, runtime.NetworkSpec{Name: "serve"}); err != nil {
		return fmt.Errorf("serve doctor: serve network: failed: %w", err)
	}
	fmt.Fprintln(stdout, "serve network: ok")
	return nil
}

func (c *Command) runRemove(ctx context.Context, args []string, stdout io.Writer) error {
	options, err := parseRemoveOptions(args)
	if err != nil {
		return err
	}
	if !options.force {
		return fmt.Errorf("serve remove: --force is required for now")
	}
	rt, err := c.runtimeFactory()
	if err != nil {
		return fmt.Errorf("serve remove: create runtime: %w", err)
	}
	containers, err := managedContainers(ctx, rt, options.filters)
	if err != nil {
		return fmt.Errorf("serve remove: list containers: %w", err)
	}
	for _, container := range containers {
		_ = rt.StopContainer(ctx, container.ID, time.Second)
		if err := rt.RemoveContainer(ctx, container.ID); err != nil {
			return fmt.Errorf("serve remove: remove %s: %w", container.Name, err)
		}
	}
	fmt.Fprintf(stdout, "Removed %d container(s)\n", len(containers))
	return nil
}

func (c *Command) runPrune(ctx context.Context, args []string, stdout io.Writer) error {
	force := false
	for _, arg := range args {
		if arg != "--force" {
			return fmt.Errorf("serve prune: unknown argument %s", arg)
		}
		force = true
	}
	if !force {
		return fmt.Errorf("serve prune: --force is required for now")
	}
	rt, err := c.runtimeFactory()
	if err != nil {
		return fmt.Errorf("serve prune: create runtime: %w", err)
	}
	containers, err := managedContainers(ctx, rt, map[string]string{})
	if err != nil {
		return fmt.Errorf("serve prune: list containers: %w", err)
	}
	removed := 0
	for _, container := range containers {
		if container.Running {
			continue
		}
		if err := rt.RemoveContainer(ctx, container.ID); err != nil {
			return fmt.Errorf("serve prune: remove %s: %w", container.Name, err)
		}
		removed++
	}
	fmt.Fprintf(stdout, "Pruned %d container(s)\n", removed)
	return nil
}

func (c *Command) runRollback(ctx context.Context, args []string, stdout io.Writer) error {
	options, err := parseRollbackOptions(args)
	if err != nil {
		return err
	}
	store := agentstate.NewStore(options.stateDir)
	desired, err := store.LoadLastGood(options.service, options.destination)
	if err != nil {
		return fmt.Errorf("serve rollback: load last-good state: %w", err)
	}
	rt, err := c.runtimeFactory()
	if err != nil {
		return fmt.Errorf("serve rollback: create runtime: %w", err)
	}

	sink := events.NewJSONSink(stdout)
	rollbackEvent := healing.LifecycleEvent{
		Service:     desired.Service,
		Destination: desired.Destination,
		Version:     desired.Version,
		Actor:       "serve",
	}
	rollbackEvent.Name = "rollback_started"
	if err := sink.Emit(ctx, rollbackEvent); err != nil {
		return fmt.Errorf("serve rollback: %w", err)
	}
	// applyDesired runs the health-gated cutover engine: if the last-good
	// version never becomes healthy, traffic stays on the current version.
	if err := applyDesired(ctx, rt, desired, options.stateDir); err != nil {
		return fmt.Errorf("serve rollback: %w", err)
	}
	rollbackEvent.Name = "rollback_completed"
	if err := sink.Emit(ctx, rollbackEvent); err != nil {
		return fmt.Errorf("serve rollback: %w", err)
	}
	fmt.Fprintf(stdout, "Rolled back %s %s to %s\n", desired.Service, desired.Destination, desired.Version)
	return nil
}

func (c *Command) runSecrets(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("serve secrets: expected subcommand")
	}
	if args[0] != "edit" {
		return fmt.Errorf("serve secrets %s is not implemented yet", args[0])
	}
	path := "serve.secrets.yml"
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--file":
			if i+1 >= len(args) {
				return fmt.Errorf("serve secrets edit: --file requires a value")
			}
			path = args[i+1]
			i++
		default:
			return fmt.Errorf("serve secrets edit: unknown argument %s", args[i])
		}
	}
	if err := c.runner.Run(ctx, "sops", path); err != nil {
		return fmt.Errorf("serve secrets edit: %w", err)
	}
	fmt.Fprintf(stdout, "Edited %s\n", path)
	return nil
}

type containerSelection struct {
	container string
	filters   map[string]string
}

func parseContainerSelection(command string, args []string) (containerSelection, error) {
	selection := containerSelection{filters: map[string]string{}}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--container":
			if i+1 >= len(args) {
				return selection, fmt.Errorf("%s: --container requires a value", command)
			}
			selection.container = args[i+1]
			i++
		case "--service":
			if i+1 >= len(args) {
				return selection, fmt.Errorf("%s: --service requires a value", command)
			}
			selection.filters["serve.service"] = args[i+1]
			i++
		case "--destination":
			if i+1 >= len(args) {
				return selection, fmt.Errorf("%s: --destination requires a value", command)
			}
			selection.filters["serve.destination"] = args[i+1]
			i++
		case "--role":
			if i+1 >= len(args) {
				return selection, fmt.Errorf("%s: --role requires a value", command)
			}
			selection.filters["serve.role"] = args[i+1]
			i++
		default:
			return selection, fmt.Errorf("%s: unknown argument %s", command, args[i])
		}
	}
	return selection, nil
}

func selectContainer(ctx context.Context, rt runtime.Runtime, selection containerSelection) (runtime.ContainerState, error) {
	containers, err := managedContainers(ctx, rt, selection.filters)
	if err != nil {
		return runtime.ContainerState{}, err
	}
	if selection.container != "" {
		for _, container := range containers {
			if container.Name == selection.container || string(container.ID) == selection.container {
				return container, nil
			}
		}
		return runtime.ContainerState{}, fmt.Errorf("container %s not found", selection.container)
	}
	if len(containers) == 0 {
		return runtime.ContainerState{}, fmt.Errorf("no matching Serve-managed containers found")
	}
	if len(containers) > 1 {
		return runtime.ContainerState{}, fmt.Errorf("multiple matching containers found; pass --container")
	}
	return containers[0], nil
}

func managedContainers(ctx context.Context, rt runtime.Runtime, labels map[string]string) ([]runtime.ContainerState, error) {
	filters := map[string]string{"serve.managed": "true"}
	for key, value := range labels {
		if value != "" {
			filters[key] = value
		}
	}
	containers, err := rt.ListContainers(ctx, runtime.ContainerFilters{Labels: filters})
	if err != nil {
		return nil, err
	}
	sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })
	return containers, nil
}

func printStatus(stdout io.Writer, containers []runtime.ContainerState) error {
	writer := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "SERVICE\tDESTINATION\tROLE\tVERSION\tCONTAINER\tSTATUS")
	for _, container := range containers {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n",
			container.Labels["serve.service"],
			container.Labels["serve.destination"],
			container.Labels["serve.role"],
			container.Labels["serve.version"],
			container.Name,
			status(container),
		)
	}
	return writer.Flush()
}

type removeOptions struct {
	force   bool
	filters map[string]string
}

func parseRemoveOptions(args []string) (removeOptions, error) {
	options := removeOptions{filters: map[string]string{}}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--force":
			options.force = true
		case "--service":
			if i+1 >= len(args) {
				return options, fmt.Errorf("serve remove: --service requires a value")
			}
			options.filters["serve.service"] = args[i+1]
			i++
		case "--destination":
			if i+1 >= len(args) {
				return options, fmt.Errorf("serve remove: --destination requires a value")
			}
			options.filters["serve.destination"] = args[i+1]
			i++
		case "--role":
			if i+1 >= len(args) {
				return options, fmt.Errorf("serve remove: --role requires a value")
			}
			options.filters["serve.role"] = args[i+1]
			i++
		default:
			return options, fmt.Errorf("serve remove: unknown argument %s", args[i])
		}
	}
	return options, nil
}

type rollbackOptions struct {
	service     string
	destination string
	stateDir    string
}

func parseRollbackOptions(args []string) (rollbackOptions, error) {
	options := rollbackOptions{stateDir: ".serve/state"}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--service":
			if i+1 >= len(args) {
				return options, fmt.Errorf("serve rollback: --service requires a value")
			}
			options.service = args[i+1]
			i++
		case "--destination":
			if i+1 >= len(args) {
				return options, fmt.Errorf("serve rollback: --destination requires a value")
			}
			options.destination = args[i+1]
			i++
		case "--state-dir":
			if i+1 >= len(args) {
				return options, fmt.Errorf("serve rollback: --state-dir requires a value")
			}
			options.stateDir = args[i+1]
			i++
		default:
			return options, fmt.Errorf("serve rollback: unknown argument %s", args[i])
		}
	}
	if options.service == "" {
		return options, fmt.Errorf("serve rollback: --service is required")
	}
	if options.destination == "" {
		return options, fmt.Errorf("serve rollback: --destination is required")
	}
	return options, nil
}

func (c *Command) runAgent(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("serve agent: expected subcommand")
	}
	switch args[0] {
	case "run":
		return c.runAgentDaemon(ctx, args[1:], stdout)
	case "reconcile":
		return runAgentReconcile(ctx, args[1:], stdout)
	case "status":
		return runAgentStatus(ctx, args[1:], stdout)
	case "logs":
		return runAgentLogs(ctx, args[1:], stdout)
	case "events":
		return runAgentEvents(ctx, args[1:], stdout)
	}
	if args[0] != "apply" {
		return fmt.Errorf("serve agent %s is not implemented yet", args[0])
	}
	options, err := parseAgentApplyOptions(args[1:])
	if err != nil {
		return err
	}

	desired, err := loadDesiredState(options.path)
	if err != nil {
		return fmt.Errorf("serve agent apply: %w", err)
	}
	rt, err := c.runtimeFactory()
	if err != nil {
		return fmt.Errorf("serve agent apply: create runtime: %w", err)
	}
	if err := applyDesired(ctx, rt, desired, options.stateDir); err != nil {
		return fmt.Errorf("serve agent apply: %w", err)
	}
	fmt.Fprintf(stdout, "Applied desired state for %s %s %s\n", desired.Service, desired.Destination, desired.Version)
	return nil
}

func (c *Command) runAgentDaemon(ctx context.Context, args []string, stdout io.Writer) error {
	options := agentRunOptions{
		stateDir:          daemon.DefaultStateDir,
		socketPath:        daemon.DefaultSocketPath,
		reconcileInterval: daemon.DefaultReconcileInterval,
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--state-dir":
			if i+1 >= len(args) {
				return fmt.Errorf("serve agent run: --state-dir requires a value")
			}
			i++
			options.stateDir = args[i]
		case "--socket":
			if i+1 >= len(args) {
				return fmt.Errorf("serve agent run: --socket requires a value")
			}
			i++
			options.socketPath = args[i]
		case "--reconcile-interval":
			if i+1 >= len(args) {
				return fmt.Errorf("serve agent run: --reconcile-interval requires a value")
			}
			i++
			interval, err := time.ParseDuration(args[i])
			if err != nil {
				return fmt.Errorf("serve agent run: parse --reconcile-interval: %w", err)
			}
			options.reconcileInterval = interval
		default:
			return fmt.Errorf("serve agent run: unknown flag %q", args[i])
		}
	}

	rt, err := c.runtimeFactory()
	if err != nil {
		return fmt.Errorf("serve agent run: create runtime: %w", err)
	}
	fmt.Fprintf(stdout, "serve-agent listening on %s (state dir %s)\n", options.socketPath, options.stateDir)
	return daemon.New(daemon.Config{
		Runtime:           rt,
		StateDir:          options.stateDir,
		SocketPath:        options.socketPath,
		ReconcileInterval: options.reconcileInterval,
	}).Run(ctx)
}

type agentRunOptions struct {
	stateDir          string
	socketPath        string
	reconcileInterval time.Duration
}

// agentSocketRequest performs one HTTP request against the daemon's Unix
// socket and returns the response after checking for a 200.
func agentSocketRequest(ctx context.Context, socketPath string, method string, path string) (*http.Response, error) {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _ string, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}}
	request, err := http.NewRequestWithContext(ctx, method, "http://serve-agent"+path, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		response.Body.Close()
		return nil, fmt.Errorf("agent returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	return response, nil
}

// parseHostFlag consumes --host from args, returning the host and the
// remaining arguments.
func parseHostFlag(command string, args []string) (string, []string, error) {
	host := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--host" {
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("%s: --host requires a value", command)
			}
			i++
			host = args[i]
			continue
		}
		rest = append(rest, args[i])
	}
	if host != "" {
		if err := config.ValidateSSHHost(host); err != nil {
			return "", nil, fmt.Errorf("%s: %w", command, err)
		}
	}
	return host, rest, nil
}

// parseSocketFlag consumes --socket from args, returning the socket path
// and the remaining arguments.
func parseSocketFlag(command string, args []string) (string, []string, error) {
	socketPath := daemon.DefaultSocketPath
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--socket" {
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("%s: --socket requires a value", command)
			}
			i++
			socketPath = args[i]
			continue
		}
		rest = append(rest, args[i])
	}
	return socketPath, rest, nil
}

// runAgentReconcile pokes the local agent daemon over its Unix socket so it
// picks up desired-state files written to disk.
func runAgentReconcile(ctx context.Context, args []string, stdout io.Writer) error {
	socketPath, rest, err := parseSocketFlag("serve agent reconcile", args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("serve agent reconcile: unknown flag %q", rest[0])
	}
	response, err := agentSocketRequest(ctx, socketPath, http.MethodPost, "/v1/reconcile")
	if err != nil {
		return fmt.Errorf("serve agent reconcile: %w", err)
	}
	response.Body.Close()
	fmt.Fprintln(stdout, "Agent reconciled")
	return nil
}

func runAgentStatus(ctx context.Context, args []string, stdout io.Writer) error {
	socketPath, rest, err := parseSocketFlag("serve agent status", args)
	if err != nil {
		return err
	}
	rawJSON := false
	for _, arg := range rest {
		if arg != "--json" {
			return fmt.Errorf("serve agent status: unknown flag %q", arg)
		}
		rawJSON = true
	}

	response, err := agentSocketRequest(ctx, socketPath, http.MethodGet, "/v1/status")
	if err != nil {
		return fmt.Errorf("serve agent status: %w", err)
	}
	defer response.Body.Close()

	if rawJSON {
		_, err = io.Copy(stdout, response.Body)
		return err
	}

	var states []agentstate.ActualState
	if err := json.NewDecoder(response.Body).Decode(&states); err != nil {
		return fmt.Errorf("serve agent status: decode response: %w", err)
	}
	writer := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "SERVICE\tDESTINATION\tROLE\tVERSION\tCONTAINER\tSTATUS")
	for _, state := range states {
		for _, container := range state.Containers {
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n", state.Service, state.Destination, container.Role, container.Version, container.Name, container.Status)
		}
	}
	return writer.Flush()
}

func runAgentLogs(ctx context.Context, args []string, stdout io.Writer) error {
	socketPath, rest, err := parseSocketFlag("serve agent logs", args)
	if err != nil {
		return err
	}
	container := ""
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--container":
			if i+1 >= len(rest) {
				return fmt.Errorf("serve agent logs: --container requires a value")
			}
			i++
			container = rest[i]
		default:
			return fmt.Errorf("serve agent logs: unknown flag %q", rest[i])
		}
	}
	if container == "" {
		return fmt.Errorf("serve agent logs: --container is required")
	}

	response, err := agentSocketRequest(ctx, socketPath, http.MethodGet, "/v1/logs?container="+url.QueryEscape(container))
	if err != nil {
		return fmt.Errorf("serve agent logs: %w", err)
	}
	defer response.Body.Close()
	_, err = io.Copy(stdout, response.Body)
	return err
}

func runAgentEvents(ctx context.Context, args []string, stdout io.Writer) error {
	socketPath, rest, err := parseSocketFlag("serve agent events", args)
	if err != nil {
		return err
	}
	once := false
	for _, arg := range rest {
		if arg != "--once" {
			return fmt.Errorf("serve agent events: unknown flag %q", arg)
		}
		once = true
	}

	response, err := agentSocketRequest(ctx, socketPath, http.MethodGet, "/v1/events")
	if err != nil {
		return fmt.Errorf("serve agent events: %w", err)
	}
	defer response.Body.Close()

	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		fmt.Fprintln(stdout, scanner.Text())
		if once {
			return nil
		}
	}
	return scanner.Err()
}

func (c *Command) runDeploy(ctx context.Context, args []string, stdout io.Writer) error {
	options, err := parseDeployOptions(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(options.configPath)
	if err != nil {
		return fmt.Errorf("serve deploy: %w", err)
	}
	secretsFile, err := secretsFileContent(cfg, options.configPath)
	if err != nil {
		return fmt.Errorf("serve deploy: %w", err)
	}
	if !options.local {
		return c.remoteDeploy(ctx, cfg, options, secretsFile, stdout)
	}
	desired, err := planner.Plan(cfg, planner.Options{Host: options.host, Version: options.version, SecretsFileContent: secretsFile})
	if err != nil {
		return fmt.Errorf("serve deploy: plan desired state: %w", err)
	}
	rt, err := c.runtimeFactory()
	if err != nil {
		return fmt.Errorf("serve deploy: create runtime: %w", err)
	}
	if err := applyDesired(ctx, rt, desired, options.stateDir); err != nil {
		return fmt.Errorf("serve deploy: %w", err)
	}
	fmt.Fprintf(stdout, "Deployed %s %s %s locally\n", desired.Service, desired.Destination, desired.Version)
	return nil
}

// remoteDeploy computes per-host desired state and feeds it to a transactional
// agent apply over SSH. The remote command exits unsuccessfully when cutover
// fails, and desired state is only promoted after a successful apply.
func (c *Command) remoteDeploy(ctx context.Context, cfg config.Config, options deployOptions, secretsFile string, stdout io.Writer) error {
	hosts := configHosts(cfg)
	if len(hosts) == 0 {
		return fmt.Errorf("serve deploy: no hosts configured")
	}

	for _, host := range hosts {
		desired, err := planner.Plan(cfg, planner.Options{Host: host, Version: options.version, SecretsFileContent: secretsFile})
		if err != nil {
			return fmt.Errorf("serve deploy: plan desired state for %s: %w", host, err)
		}
		payload, err := json.MarshalIndent(desired, "", "  ")
		if err != nil {
			return fmt.Errorf("serve deploy: encode desired state for %s: %w", host, err)
		}

		apply := fmt.Sprintf("sudo serve agent apply /dev/stdin --state-dir %s", daemon.DefaultStateDir)
		if err := c.sshRunner.Run(ctx, host, apply, bytes.NewReader(payload), nil); err != nil {
			return fmt.Errorf("serve deploy: apply desired state on %s: %w", host, err)
		}
		fmt.Fprintf(stdout, "Deployed %s %s %s to %s\n", desired.Service, desired.Destination, desired.Version, host)
	}
	return nil
}

// secretsFileContent reads the encrypted secrets file so it can travel
// inside the desired state (SOPS ciphertext only). The path mirrors the
// planner's secrets_ref: serve.secrets.yml next to the config file.
func secretsFileContent(cfg config.Config, configPath string) (string, error) {
	if len(cfg.Env.Secret) == 0 {
		return "", nil
	}
	path := filepath.Join(filepath.Dir(configPath), "serve.secrets.yml")
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read secrets file %s (required because env.secret is configured): %w", path, err)
	}
	return string(contents), nil
}

func configHosts(cfg config.Config) []string {
	seen := map[string]struct{}{}
	var hosts []string
	add := func(candidates []string) {
		for _, host := range candidates {
			trimmed := strings.TrimSpace(host)
			if trimmed == "" {
				continue
			}
			if _, ok := seen[trimmed]; ok {
				continue
			}
			seen[trimmed] = struct{}{}
			hosts = append(hosts, trimmed)
		}
	}
	for _, role := range sortedKeys(cfg.Servers) {
		add(cfg.Servers[role].Hosts)
	}
	for _, name := range sortedKeys(cfg.Accessories) {
		add(cfg.Accessories[name].Hosts)
	}
	return hosts
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func applyDesired(ctx context.Context, rt runtime.Runtime, desired planner.DesiredState, stateDir string) error {
	if err := agentstate.ValidateIdentity(desired.Service, desired.Destination); err != nil {
		return err
	}
	store := agentstate.NewStore(stateDir)
	engine := cutover.New(cutover.Deps{
		Runtime:  rt,
		Starter:  starterFor(rt, desired),
		Health:   health.NewHTTPChecker(nil),
		Proxy:    kamalproxy.New(rt, kamalproxy.Options{Network: desired.Network}),
		LastGood: store,
	})
	if err := engine.Apply(ctx, desired); err != nil {
		return err
	}
	actual, err := actualState(ctx, rt, desired.Service, desired.Destination)
	if err != nil {
		return err
	}
	if err := store.SaveActual(actual); err != nil {
		return err
	}
	return store.SaveDesired(desired)
}

func starterFor(rt runtime.Runtime, desired planner.DesiredState) *reconciler.Reconciler {
	for _, container := range desired.Containers {
		if len(container.SecretNames) > 0 {
			return reconciler.NewWithSecrets(rt, sops.NewDefaultStore(), secrets.NewEnvFileWriter(secretEnvFileDir))
		}
	}
	return reconciler.New(rt)
}

func actualState(ctx context.Context, rt runtime.Runtime, service string, destination string) (agentstate.ActualState, error) {
	containers, err := rt.ListContainers(ctx, runtime.ContainerFilters{Labels: map[string]string{
		"serve.managed":     "true",
		"serve.service":     service,
		"serve.destination": destination,
	}})
	if err != nil {
		return agentstate.ActualState{}, err
	}
	actual := agentstate.ActualState{Service: service, Destination: destination}
	for _, container := range containers {
		actual.Containers = append(actual.Containers, agentstate.ActualContainer{
			Name:    container.Name,
			Role:    container.Labels["serve.role"],
			Version: container.Labels["serve.version"],
			Status:  status(container),
		})
	}
	return actual, nil
}

func loadDesiredState(path string) (planner.DesiredState, error) {
	file, err := os.Open(path)
	if err != nil {
		return planner.DesiredState{}, fmt.Errorf("open desired state %s: %w", path, err)
	}
	defer file.Close()
	var desired planner.DesiredState
	if err := json.NewDecoder(file).Decode(&desired); err != nil {
		return planner.DesiredState{}, fmt.Errorf("decode desired state %s: %w", path, err)
	}
	return desired, nil
}

type agentApplyOptions struct {
	path     string
	stateDir string
}

func parseAgentApplyOptions(args []string) (agentApplyOptions, error) {
	options := agentApplyOptions{stateDir: ".serve/state"}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--state-dir":
			if i+1 >= len(args) {
				return options, fmt.Errorf("serve agent apply: --state-dir requires a value")
			}
			options.stateDir = args[i+1]
			i++
		default:
			if strings.HasPrefix(args[i], "-") {
				return options, fmt.Errorf("serve agent apply: unknown argument %s", args[i])
			}
			if options.path != "" {
				return options, fmt.Errorf("serve agent apply: multiple desired state files provided")
			}
			options.path = args[i]
		}
	}
	if options.path == "" {
		return options, fmt.Errorf("serve agent apply: desired state path is required")
	}
	return options, nil
}

type deployOptions struct {
	local      bool
	configPath string
	host       string
	version    string
	stateDir   string
}

func parseDeployOptions(args []string) (deployOptions, error) {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "localhost"
	}
	options := deployOptions{configPath: "serve.yml", host: host, version: "dev", stateDir: ".serve/state"}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--local":
			options.local = true
		case "--config":
			if i+1 >= len(args) {
				return options, fmt.Errorf("serve deploy: --config requires a value")
			}
			options.configPath = args[i+1]
			i++
		case "--host":
			if i+1 >= len(args) {
				return options, fmt.Errorf("serve deploy: --host requires a value")
			}
			options.host = args[i+1]
			i++
		case "--version":
			if i+1 >= len(args) {
				return options, fmt.Errorf("serve deploy: --version requires a value")
			}
			options.version = args[i+1]
			i++
		case "--state-dir":
			if i+1 >= len(args) {
				return options, fmt.Errorf("serve deploy: --state-dir requires a value")
			}
			options.stateDir = args[i+1]
			i++
		default:
			return options, fmt.Errorf("serve deploy: unknown argument %s", args[i])
		}
	}
	return options, nil
}

func status(container runtime.ContainerState) string {
	if container.Running {
		return "running"
	}
	return "stopped"
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// execSSHRunner shells out to the system ssh binary so the operator's SSH
// config, keys, and known_hosts apply.
type execSSHRunner struct{}

func (execSSHRunner) Run(ctx context.Context, host string, command string, stdin io.Reader, stdout io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	cmd := exec.CommandContext(ctx, "ssh", "--", host, command)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func printHelp(w io.Writer) {
	fmt.Fprint(w, `Usage:
  serve <command>

Implemented commands:
  init              Create a starter serve.yml
  status            Show local Serve-managed Docker containers
  logs              Stream container logs
  events            Stream Docker runtime events
  doctor            Validate local Docker basics
  remove            Remove local Serve-managed containers
  prune             Remove stopped Serve-managed containers
  rollback          Apply the stored last-good desired state
  secrets edit      Edit SOPS-encrypted secrets
  agent apply       Apply a desired-state JSON locally
  agent run         Run the long-lived host agent daemon
  agent reconcile   Poke the local agent daemon to reconcile now
  agent status      Show status from the local agent daemon
  agent logs        Stream container logs from the local agent daemon
  agent events      Stream runtime events from the local agent daemon
  deploy            Upload desired state to remote hosts over SSH
  deploy --local    Plan and apply a local desired state
  exec              Run a command in a managed container (--host for remote)
  version           Print build version
  help              Show this help

Remote variants: status --config, logs --host, events --host, exec --host.

Not implemented yet:
  setup             Prepare a remote VM
`)
}

const starterConfig = `service: my-app
image: ghcr.io/acme/my-app
destination: production

servers:
  web:
    hosts:
      - localhost
    command: ./server
    app_port: 3000
    replicas: 1

networking:
  private_network: serve

retain_containers: 5
`
