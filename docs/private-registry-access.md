# Private Registry Access

Serve uses the standard Docker client configuration for authenticated image pulls. It reads `$DOCKER_CONFIG/config.json` when `DOCKER_CONFIG` is set, otherwise it reads `$HOME/.docker/config.json`.

The configuration may contain inline `auths`, a global `credsStore`, or registry-specific `credHelpers`. Credential helper binaries must be installed on the deployment host and available in the Serve agent's `PATH`.

Use a dedicated, read-only identity for each deployment host:

- GCP Artifact Registry: grant the attached VM service account `roles/artifactregistry.reader` and configure its Docker credential helper.
- AWS ECR: use an instance role with pull-only ECR permissions and configure the ECR credential helper.
- GHCR: use a token with `read:packages` and access only to the required package.
- Public registries require no authentication.

The systemd agent runs as root, so its default configuration is `/root/.docker/config.json`. For GHCR:

```sh
printf '%s' "$GHCR_TOKEN" |
  sudo -H docker login ghcr.io --username USERNAME --password-stdin
```

Serve reloads the Docker configuration for each pull; the agent does not need to be restarted after login or credential rotation.

To use another configuration directory, add a systemd override with an absolute path:

```sh
sudo systemctl edit serve-agent
```

```ini
[Service]
Environment=DOCKER_CONFIG=/etc/serve/docker
```

Then reload and restart the unit:

```sh
sudo systemctl daemon-reload
sudo systemctl restart serve-agent
```

For `serve deploy --local`, Serve uses the invoking user's Docker configuration instead.

Credentials remain host-side and are passed in memory to the Docker API. They must not appear in `serve.yml`, desired state, logs, or command-line arguments. Serve does not install credential helpers, create cloud identities, or run `docker login` for you.

Serve still tolerates a pull failure when the exact image is already available locally. If the image is unavailable, the deployment error includes both the pull failure and Docker's container creation failure.

[Back to the README](../README.md)
