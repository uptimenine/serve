package docker

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	"github.com/distribution/reference"
	cliconfig "github.com/docker/cli/cli/config"
	registrytypes "github.com/docker/docker/api/types/registry"
)

type registryAuthProvider interface {
	RegistryAuth(ctx context.Context, imageRef string) (string, error)
}

type dockerConfigAuthProvider struct {
	configDir string
	pathErr   error
}

func newDockerConfigAuthProvider() *dockerConfigAuthProvider {
	configDir, err := defaultDockerConfigDir()
	return &dockerConfigAuthProvider{configDir: configDir, pathErr: err}
}

func (p *dockerConfigAuthProvider) RegistryAuth(ctx context.Context, imageRef string) (string, error) {
	_ = ctx
	if p.pathErr != nil {
		return "", p.pathErr
	}

	cfg, err := cliconfig.Load(p.configDir)
	if err != nil {
		return "", fmt.Errorf("load Docker config from %s: %w", p.configDir, err)
	}
	configKey, err := registryConfigKey(imageRef)
	if err != nil {
		return "", err
	}
	auth, err := cfg.GetAuthConfig(configKey)
	if err != nil {
		return "", fmt.Errorf("resolve Docker credentials for %s: %w", configKey, err)
	}
	encoded, err := registrytypes.EncodeAuthConfig(registrytypes.AuthConfig(auth))
	if err != nil {
		return "", fmt.Errorf("encode Docker credentials for %s: %w", configKey, err)
	}
	return encoded, nil
}

func registryConfigKey(imageRef string) (string, error) {
	named, err := reference.ParseNormalizedNamed(imageRef)
	if err != nil {
		return "", fmt.Errorf("parse image reference %q: %w", imageRef, err)
	}
	registryHost := reference.Domain(named)
	if registryHost == "docker.io" {
		return "https://index.docker.io/v1/", nil
	}
	return registryHost, nil
}

func defaultDockerConfigDir() (string, error) {
	if configDir := os.Getenv(cliconfig.EnvOverrideConfigDir); configDir != "" {
		return configDir, nil
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".docker"), nil
	}
	current, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for Docker config: %w", err)
	}
	if current.HomeDir == "" {
		return "", fmt.Errorf("resolve home directory for Docker config: home directory is empty")
	}
	return filepath.Join(current.HomeDir, ".docker"), nil
}
