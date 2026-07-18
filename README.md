# Serve

Serve deploys Docker applications to your own servers over SSH. It performs health-checked rolling deployments, keeps applications running through a host agent, and routes HTTP and HTTPS traffic through one shared `kamal-proxy` instance per machine.

## Install

Prebuilt releases are available for Linux AMD64. Install the binary on the computer you deploy from and on every deployment host:

```sh
curl -fLO https://github.com/uptimenine/serve/releases/latest/download/serve-linux-amd64
curl -fLO https://github.com/uptimenine/serve/releases/latest/download/SHA256SUMS
sha256sum --check SHA256SUMS
sudo install -m 0755 serve-linux-amd64 /usr/local/bin/serve
serve version
```

For other platforms, [build Serve from source](docs/contributing.md#build-a-local-binary).

## Prepare a server

Each deployment host needs:

- Linux with Docker running
- the Serve binary at `/usr/local/bin/serve`
- the Serve systemd unit
- ports 80 and 443 available for `kamal-proxy`
- SSH access from your deployment computer
- passwordless `sudo` access to `serve` for the deployment user

Install and start the agent once on each host:

```sh
curl -fL https://raw.githubusercontent.com/uptimenine/serve/main/packaging/systemd/serve-agent.service \
  -o serve-agent.service
sudo install -m 0644 serve-agent.service /etc/systemd/system/serve-agent.service
sudo systemctl daemon-reload
sudo systemctl enable --now serve-agent
sudo systemctl status serve-agent
```

Allow the SSH deployment user to invoke Serve without a password. For a user named `deploy`, create `/etc/sudoers.d/serve` with `visudo`:

```sudoers
deploy ALL=(root) NOPASSWD: /usr/local/bin/serve
```

The agent creates and manages the machine's shared `kamal-proxy` container when the first routed application is deployed. You do not need to start the proxy separately.

## Deploy an application

Create a directory for the application:

```sh
mkdir my-app
cd my-app
serve init
```

Edit `serve.yml`:

```yaml
service: my-app
image: ghcr.io/acme/my-app
destination: production

servers:
  web:
    hosts:
      - deploy@example.com
    command: ./server
    app_port: 3000
    replicas: 2
    healthcheck:
      http:
        path: /up
        port: 3000
      interval: 2s
      timeout: 2s
      retries: 10

proxy:
  provider: kamal-proxy
  app_role: web
  hosts:
    - api.example.com
  ssl: auto

networking:
  private_network: serve

retain_containers: 5
```

The host value is an SSH destination and may include a user, such as `deploy@example.com`. Point the proxy hostnames at the server before enabling automatic TLS.

Push the application image, then deploy its tag:

```sh
serve deploy --config serve.yml --version v1.2.3
```

If `image` has no tag, Serve appends the value passed to `--version`. The example above deploys `ghcr.io/acme/my-app:v1.2.3`.

Serve starts the new containers, waits for their health checks, switches proxy traffic, and then retires the previous containers according to `retain_containers`.

## Operate the application

Show containers on every host in `serve.yml`:

```sh
serve status --config serve.yml
```

Stream logs from a container:

```sh
serve logs --host deploy@example.com --container my-app-web-production-v1.2.3-r1
```

Run a command in a container:

```sh
serve exec --host deploy@example.com \
  --container my-app-web-production-v1.2.3-r1 -- env
```

Stream Docker events from a host:

```sh
serve events --host deploy@example.com
```

Run `serve --help` for the complete command list.

## Multiple applications and domains

Use a separate `serve.yml` for each application. All applications deployed to the same machine share its Serve agent, Docker network, and central `kamal-proxy` container.

One application can answer on multiple domains:

```yaml
proxy:
  provider: kamal-proxy
  app_role: web
  hosts:
    - api.x.com
    - api.you.com
  ssl: auto
```

Different applications can also use different domains on the same machine. Keep `service` names and proxy hostnames unique between applications.

## Private images and secrets

Authenticate Docker to private registries on every deployment host before deploying. Serve reads the host's standard Docker client configuration, including credential helpers, but does not provision credentials. See [Private registry access](docs/private-registry-access.md).

Application secrets can be stored in `serve.secrets.yml` with SOPS and referenced through `env.secret`. Hosts that decrypt secrets must have SOPS and the appropriate decryption credentials installed.

## Documentation

- [Getting started and command reference](docs/getting-started.md)
- [Private registry access](docs/private-registry-access.md)
- [How to contribute](docs/contributing.md)
