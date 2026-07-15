package planner

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/uptimenine/serve/internal/config"
)

type Options struct {
	Host               string
	Version            string
	Destination        string
	SecretsRef         string
	SecretCiphertext   map[string]string
	SecretsFileContent string // whole encrypted secrets file, embedded for host-side decryption
}

type DesiredState struct {
	Service          string      `json:"service"`
	Destination      string      `json:"destination"`
	Host             string      `json:"host"`
	Version          string      `json:"version"`
	Network          string      `json:"network"`
	RetainContainers int         `json:"retain_containers"`
	SecretsFile      string      `json:"secrets_file,omitempty"` // SOPS ciphertext, never plaintext
	Proxy            ProxyRoute  `json:"proxy,omitempty"`
	Containers       []Container `json:"containers"`
}

// ProxyRoute carries the public routing settings for the app role.
type ProxyRoute struct {
	Hosts         []string `json:"hosts,omitempty"`
	SSL           bool     `json:"ssl,omitempty"`
	DeployTimeout string   `json:"deploy_timeout,omitempty"`
	DrainTimeout  string   `json:"drain_timeout,omitempty"`
}

type Container struct {
	Name             string            `json:"name"`
	Role             string            `json:"role"`
	ContainerType    string            `json:"container_type"`
	Image            string            `json:"image"`
	Command          []string          `json:"command,omitempty"`
	Ports            []Port            `json:"ports,omitempty"`
	Replica          int               `json:"replica"`
	Proxy            bool              `json:"proxy,omitempty"`
	SecretsRef       string            `json:"secrets_ref,omitempty"`
	SecretNames      []string          `json:"secret_names,omitempty"`
	SecretCiphertext map[string]string `json:"secret_ciphertext,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	Healthcheck      *Healthcheck      `json:"healthcheck,omitempty"`
	Restart          Restart           `json:"restart,omitempty"`
	Aliases          []string          `json:"aliases,omitempty"`
	Volumes          []string          `json:"volumes,omitempty"`
	Labels           map[string]string `json:"labels"`
}

type Port struct {
	Name          string `json:"name"`
	ContainerPort int    `json:"container_port"`
}

type Healthcheck struct {
	Type     string `json:"type"`
	Path     string `json:"path,omitempty"`
	Port     int    `json:"port,omitempty"`
	Interval string `json:"interval,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
	Retries  int    `json:"retries,omitempty"`
}

type Restart struct {
	Policy         string `json:"policy,omitempty"`
	Controller     string `json:"controller,omitempty"`
	InitialBackoff string `json:"initial_backoff,omitempty"`
	MaxBackoff     string `json:"max_backoff,omitempty"`
	MaxAttempts    int    `json:"max_attempts,omitempty"`
	Window         string `json:"window,omitempty"`
}

type GitState struct {
	SHA   string
	Dirty bool
}

var namePartPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func Plan(cfg config.Config, opts Options) (DesiredState, error) {
	if strings.TrimSpace(opts.Host) == "" {
		return DesiredState{}, fmt.Errorf("host is required")
	}
	if strings.TrimSpace(opts.Version) == "" {
		return DesiredState{}, fmt.Errorf("version is required")
	}
	if !namePartPattern.MatchString(opts.Version) {
		return DesiredState{}, fmt.Errorf("version %q cannot be used in a container name", opts.Version)
	}

	destination := opts.Destination
	if destination == "" {
		destination = cfg.Destination
	}
	if destination == "" {
		destination = "default"
	}

	network := cfg.Networking.PrivateNetwork
	if network == "" {
		network = "serve"
	}

	retainContainers := cfg.RetainContainers
	if retainContainers == 0 {
		retainContainers = 5
	}

	state := DesiredState{
		Service:          cfg.Service,
		Destination:      destination,
		Host:             opts.Host,
		Version:          opts.Version,
		Network:          network,
		RetainContainers: retainContainers,
	}
	if len(cfg.Env.Secret) > 0 {
		state.SecretsFile = opts.SecretsFileContent
	}
	state.Proxy = ProxyRoute{
		Hosts:         append([]string(nil), cfg.Proxy.Hosts...),
		SSL:           cfg.Proxy.SSL == "auto" || cfg.Proxy.SSL == "true",
		DeployTimeout: durationString(cfg.Proxy.DeployTimeout),
		DrainTimeout:  durationString(cfg.Proxy.DrainTimeout),
	}

	for _, role := range sortedServerRoles(cfg.Servers) {
		server := cfg.Servers[role]
		if !hostMatches(server.Hosts, opts.Host) {
			continue
		}

		replicas := server.Replicas
		if replicas == 0 {
			replicas = 1
		}

		for replica := 1; replica <= replicas; replica++ {
			container := Container{
				Name:          containerName(cfg.Service, role, destination, opts.Version, replica),
				Role:          role,
				ContainerType: "app",
				Image:         imageWithVersion(cfg.Image, opts.Version),
				Command:       commandArgv(server.Command),
				Replica:       replica,
				Proxy:         role == appRole(cfg),
				Env:           copyStringMap(cfg.Env.Clear),
				Healthcheck:   healthcheck(server.Healthcheck),
				Restart:       restart(server.Restart),
				Labels:        labels(cfg.Service, destination, role, opts.Version, replica, "app"),
			}
			if server.AppPort > 0 {
				container.Ports = []Port{{Name: "http", ContainerPort: server.AppPort}}
			}
			applySecrets(&container, cfg, opts)
			state.Containers = append(state.Containers, container)
		}
	}

	for _, name := range sortedAccessoryNames(cfg.Accessories) {
		accessory := cfg.Accessories[name]
		if !hostMatches(accessory.Hosts, opts.Host) {
			continue
		}

		container := Container{
			Name:          containerName(cfg.Service, name, destination, opts.Version, 1),
			Role:          name,
			ContainerType: "accessory",
			Image:         accessory.Image,
			Replica:       1,
			Restart:       restart(accessory.Restart),
			Aliases:       append([]string(nil), accessory.Aliases...),
			Volumes:       append([]string(nil), accessory.Volumes...),
			Labels:        labels(cfg.Service, destination, name, opts.Version, 1, "accessory"),
		}
		if accessory.InternalPort > 0 {
			container.Ports = []Port{{Name: "tcp", ContainerPort: accessory.InternalPort}}
		}
		state.Containers = append(state.Containers, container)
	}

	return state, nil
}

func ResolveVersion(state GitState, suffix func() string) (string, error) {
	sha := strings.TrimSpace(state.SHA)
	if sha == "" {
		return "", fmt.Errorf("git SHA is required")
	}
	if !state.Dirty {
		return sha, nil
	}

	random := strings.TrimSpace(suffix())
	if random == "" {
		return "", fmt.Errorf("dirty version suffix is required")
	}
	return sha + "_uncommitted_" + random, nil
}

func sortedServerRoles(servers map[string]config.ServerConfig) []string {
	roles := make([]string, 0, len(servers))
	for role := range servers {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles
}

func sortedAccessoryNames(accessories map[string]config.AccessoryConfig) []string {
	names := make([]string, 0, len(accessories))
	for name := range accessories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func hostMatches(hosts []string, host string) bool {
	for _, candidate := range hosts {
		if candidate == host {
			return true
		}
	}
	return false
}

func containerName(service string, role string, destination string, version string, replica int) string {
	return fmt.Sprintf("%s-%s-%s-%s-r%d", service, role, destination, version, replica)
}

func imageWithVersion(image string, version string) string {
	if strings.Contains(image, "@") {
		return image
	}
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	if lastColon > lastSlash {
		return image
	}
	return image + ":" + version
}

func commandArgv(command string) []string {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	return strings.Fields(command)
}

func appRole(cfg config.Config) string {
	if cfg.Proxy.AppRole != "" {
		return cfg.Proxy.AppRole
	}
	return "web"
}

func applySecrets(container *Container, cfg config.Config, opts Options) {
	if len(cfg.Env.Secret) == 0 {
		return
	}

	container.SecretNames = append([]string(nil), cfg.Env.Secret...)
	container.SecretsRef = opts.SecretsRef
	if container.SecretsRef == "" {
		provider := cfg.Secrets.Provider
		if provider == "" {
			provider = "sops"
		}
		container.SecretsRef = provider + ":serve.secrets.yml"
	}

	for _, name := range cfg.Env.Secret {
		ciphertext, ok := opts.SecretCiphertext[name]
		if !ok {
			continue
		}
		if container.SecretCiphertext == nil {
			container.SecretCiphertext = map[string]string{}
		}
		container.SecretCiphertext[name] = ciphertext
	}
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

func healthcheck(source config.HealthcheckConfig) *Healthcheck {
	if source.HTTP.Path == "" && source.HTTP.Port == 0 && source.Interval.Duration == 0 && source.Timeout.Duration == 0 && source.Retries == 0 {
		return nil
	}
	check := &Healthcheck{
		Interval: durationString(source.Interval),
		Timeout:  durationString(source.Timeout),
		Retries:  source.Retries,
	}
	if source.HTTP.Path != "" || source.HTTP.Port != 0 {
		check.Type = "http"
		check.Path = source.HTTP.Path
		check.Port = source.HTTP.Port
	}
	return check
}

func restart(source config.RestartConfig) Restart {
	return Restart{
		Policy:         source.Policy,
		Controller:     source.Controller,
		InitialBackoff: durationString(source.InitialBackoff),
		MaxBackoff:     durationString(source.MaxBackoff),
		MaxAttempts:    source.MaxAttempts,
		Window:         durationString(source.Window),
	}
}

func durationString(duration config.Duration) string {
	if duration.Duration == 0 {
		return ""
	}
	return duration.Duration.String()
}

func labels(service string, destination string, role string, version string, replica int, containerType string) map[string]string {
	return map[string]string{
		"serve.managed":        "true",
		"serve.service":        service,
		"serve.destination":    destination,
		"serve.role":           role,
		"serve.version":        version,
		"serve.replica":        strconv.Itoa(replica),
		"serve.container_type": containerType,
	}
}
