---
name: serve-development
description: Use when an LLM is modifying, testing, building, installing, or running the Serve deployment tool repository.
---

# Serve Development Skill

You are working on **Serve**, a Go deployment tool inspired by Kamal.

## Read first

Before changing behavior, read:

1. `README.md`
2. The package tests for the area you are changing

The current implementation and its behavior tests are the source of truth for intended architecture. Host provisioning remains governed by the manual host prerequisite policy below.

## Current product status

The core deploy loop is implemented: remote deploy over SSH, the long-running agent, event-driven healing, periodic reconciliation, health-gated blue-green cutover through kamal-proxy, SOPS secret delivery, rollback state, and remote status/logs/events/exec.

Implemented commands:

```sh
serve help
serve --help
serve -h
serve version
serve init [--path serve.yml] [--force]
serve status [--config serve.yml]
serve logs [--host HOST] [--container NAME] [--service SERVICE] [--destination DEST] [--role ROLE]
serve events [--host HOST] [--once]
serve doctor
serve remove [--service SERVICE] [--destination DEST] [--role ROLE] --force
serve prune --force
serve rollback --service SERVICE --destination DEST [--state-dir .serve/state]
serve secrets edit [--file serve.secrets.yml]
serve agent apply <desired.json> [--state-dir .serve/state]
serve agent run [--state-dir DIR] [--socket PATH] [--reconcile-interval 10s]
serve agent reconcile [--socket PATH]
serve agent status [--json] [--socket PATH]
serve agent logs --container NAME [--socket PATH]
serve agent events [--once] [--socket PATH]
serve deploy [--config serve.yml] [--version VERSION]
serve deploy --local [--config serve.yml] [--host localhost] [--version dev] [--state-dir .serve/state]
serve exec [--host HOST] --container NAME -- CMD [ARGS...]
```

`serve setup` is not a product requirement. Do not implement it unless the user explicitly changes the scope.

Do not claim an unimplemented command works. If adding command placeholders, they must return a clear `not implemented yet` message.

## TDD requirement

Follow red-green-refactor.

1. Write a failing behavior test first.
2. Run the targeted test and confirm it fails for the expected reason.
3. Implement the smallest production change.
4. Run the targeted test until it passes.
5. Run the broader suite.

Do not add production behavior without a failing test first.

## Install

From the repository root:

```sh
go install ./cmd/serve
```

Ensure Go's bin directory is on `PATH`:

```sh
export PATH="$(go env GOPATH)/bin:$PATH"
```

Verify:

```sh
serve --help
serve version
```

Alternative local build:

```sh
go build -o bin/serve ./cmd/serve
./bin/serve --help
```

Build with a version string:

```sh
go build -ldflags "-X main.version=$(git rev-parse --short HEAD)" -o bin/serve ./cmd/serve
```

## Test

Fast suite:

```sh
go test ./...
```

Make target:

```sh
make test
```

Docker integration suite:

```sh
go test -tags=integration ./...
```

Coverage:

```sh
go test -cover ./...
```

Run integration tests only when Docker is available.

## Run locally

Create or update a starter config:

```sh
serve init --path serve.yml
```

For a local smoke test, use a pullable image:

```sh
cat > serve.yml <<'YAML'
service: demo
image: busybox:1.36
destination: local

servers:
  web:
    hosts:
      - localhost
    command: sleep 3600
    replicas: 1

networking:
  private_network: serve

retain_containers: 5
YAML

docker pull busybox:1.36
serve deploy --local --config serve.yml --host localhost --version dev --state-dir .serve/state
serve status
```

Clean up:

```sh
docker rm -f demo-web-local-dev-r1
```

Apply a desired state JSON directly:

```sh
serve agent apply ./desired.json --state-dir .serve/state
```

## Host prerequisites

Host provisioning is intentionally out of scope. Docker, the Serve binary, the systemd unit, required directories, SOPS, and credentials must be installed and configured manually before remote deployment. Serve may validate prerequisites, but it must not install or provision them.

## Architecture reminders

- The CLI is the deploy controller.
- The agent is the host orchestrator.
- Docker is the runtime, not the orchestrator.
- Systemd should only start the Serve agent, not individual app containers.
- App/accessory containers are managed through the agent/reconciler.
- Secrets are delivered via tmpfs env files; never put plaintext secrets on CLI args or in logs.
- Do not add host provisioning or installation behavior to `serve setup`.

## Package map

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
internal/agent/cutover            blue-green cutover and retention engine
internal/agent/daemon             long-running agent and Unix socket API
internal/agent/events             structured lifecycle events
internal/agent/healing            restart/healing supervisor
internal/agent/health             health checker interfaces/HTTP checker
internal/agent/proxy              proxy manager interface
internal/agent/proxy/kamalproxy   kamal-proxy provider
internal/agent/secrets            env-file secret delivery
internal/agent/secrets/sops       SOPS-backed secret resolver
packaging/systemd                 manually installed Serve agent unit
```

## Development approach

Prefer vertical, runnable CLI slices over isolated internals. Choose work from the user's request and current package contracts, while keeping host provisioning out of scope.
