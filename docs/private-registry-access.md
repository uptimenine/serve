# Private Registry Access

Registry authorization is managed manually through the registry or cloud provider. Use a dedicated, read-only identity for each deployment host:

- GCP Artifact Registry: grant the attached VM service account `roles/artifactregistry.reader`.
- AWS ECR: use an instance role with pull-only ECR permissions.
- GHCR: use a token with `read:packages` and access only to the required package.
- Public registries require no authentication.

Credentials must remain host-side and must not appear as plaintext in `serve.yml`, desired state, logs, or command-line arguments.

Authenticated image pulls are not yet wired into Serve: the `registry` configuration is parsed, but the agent does not pass registry credentials to the Docker API. Until that is implemented, authenticate and pre-pull the exact private image on every host. For GHCR:

```sh
printf '%s' "$GHCR_TOKEN" |
  sudo docker login ghcr.io --username USERNAME --password-stdin

sudo docker pull ghcr.io/organization/application:VERSION
```

Serve tolerates a pull failure when the exact image is already available locally. IAM roles, registry permissions, credential helpers, and static credentials are provisioned manually; `serve setup` does not manage them.

The intended implementation is a host-side registry authentication provider supporting Docker credential helpers, cloud workload identities such as GCP ADC or AWS instance credentials, and optionally SOPS-encrypted static tokens. It will pass short-lived credentials to Docker in memory without embedding them in desired state.

[Back to the README](../README.md)
