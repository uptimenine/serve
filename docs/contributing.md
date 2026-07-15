# Contributing

## Requirements

- Go 1.26+
- Docker, for local Docker commands and integration tests
- `make`, optional but convenient

## Workflow

1. Read the package tests and surrounding implementation before changing a cross-package contract.
2. Add or update behavior-focused tests for the change.
3. Keep CLI output and documentation aligned with implemented behavior.
4. Format Go changes with `gofmt`.
5. Run `go vet ./...` and the relevant unit and integration tests before opening a pull request.

## Build a local binary

```sh
go build -o bin/serve ./cmd/serve
./bin/serve --help
```

Build with an injected version string:

```sh
go build -ldflags "-X main.version=$(git rev-parse --short HEAD)" -o bin/serve ./cmd/serve
```

During development, you can run without building:

```sh
go run ./cmd/serve --help
```

## Test

Run the default fast test suite:

```sh
go test ./...
```

Or via Make:

```sh
make test
```

Run Docker integration tests:

```sh
go test -tags=integration ./...
```

The integration suite uses the local Docker daemon and may pull/run temporary `busybox` containers.

Check coverage:

```sh
go test -cover ./...
```

## Build artifacts and versioning

Every merge or push to `main` publishes a GitHub Release containing a Linux AMD64 binary, its checksum, and a `VERSION` file. The same files are also retained as a GitHub Actions artifact for 90 days.

Builds and release tags use calendar versions in `year.mm.number` format, such as `2026.07.1`. The final number is the Build workflow's run number.

## Using this repository with another LLM

LLM-facing instructions live in:

```txt
skills/serve/SKILL.md
```

If your coding agent supports skills, add or symlink that skill into the agent's skill directory. If it does not support skills, paste the contents of `skills/serve/SKILL.md` into the LLM's system/developer instructions before asking it to work on this repository.

The skill tells an LLM to:

- follow TDD,
- keep CLI behavior honest,
- run the correct unit and integration tests,
- use the current install/build/run commands.

## Project layout

```txt
cmd/serve                         CLI entrypoint
internal/cli                      CLI command routing and local command implementations
internal/config                   serve.yml parser/validator
internal/planner                  desired-state planner
internal/runtime                  runtime interface
internal/runtime/fake             in-memory runtime for behavior tests
internal/runtime/docker           Docker Engine implementation
internal/agent/state              desired/actual/last-good state store
internal/agent/reconciler         desired-state reconciler
internal/agent/cutover            blue-green cutover engine (health gate, drain, retention)
internal/agent/daemon             long-running agent: event loop, reconcile ticker, Unix socket API
internal/agent/healing            restart/healing supervisor
internal/agent/health             health checker interfaces/HTTP checker
internal/agent/proxy              proxy manager interface
internal/agent/proxy/kamalproxy   kamal-proxy provider (docker exec)
internal/agent/events             structured JSON lifecycle event sink
internal/agent/secrets            env-file secret delivery
internal/agent/secrets/sops       SOPS-backed secret resolver
packaging/systemd                 serve-agent systemd unit
skills/serve/SKILL.md             instructions for other LLM coding agents
```

[Back to the README](../README.md)
