package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Service          string                     `yaml:"service"`
	Image            string                     `yaml:"image"`
	Destination      string                     `yaml:"destination"`
	Registry         RegistryConfig             `yaml:"registry"`
	Builder          BuilderConfig              `yaml:"builder"`
	Servers          map[string]ServerConfig    `yaml:"servers"`
	Proxy            ProxyConfig                `yaml:"proxy"`
	Networking       NetworkingConfig           `yaml:"networking"`
	RetainContainers int                        `yaml:"retain_containers"`
	Accessories      map[string]AccessoryConfig `yaml:"accessories"`
	Env              EnvConfig                  `yaml:"env"`
	Secrets          SecretsConfig              `yaml:"secrets"`
	Observability    ObservabilityConfig        `yaml:"observability"`
}

type RegistryConfig struct {
	Server   string   `yaml:"server"`
	Username string   `yaml:"username"`
	Password []string `yaml:"password"`
}

type BuilderConfig struct {
	Context    string `yaml:"context"`
	Dockerfile string `yaml:"dockerfile"`
	Arch       string `yaml:"arch"`
}

type ServerConfig struct {
	Hosts       []string          `yaml:"hosts"`
	Command     string            `yaml:"command"`
	AppPort     int               `yaml:"app_port"`
	Replicas    int               `yaml:"replicas"`
	Healthcheck HealthcheckConfig `yaml:"healthcheck"`
	Restart     RestartConfig     `yaml:"restart"`
}

type HealthcheckConfig struct {
	HTTP     HTTPHealthcheckConfig `yaml:"http"`
	Interval Duration              `yaml:"interval"`
	Timeout  Duration              `yaml:"timeout"`
	Retries  int                   `yaml:"retries"`
}

type HTTPHealthcheckConfig struct {
	Path string `yaml:"path"`
	Port int    `yaml:"port"`
}

type RestartConfig struct {
	Policy         string   `yaml:"policy"`
	Controller     string   `yaml:"controller"`
	InitialBackoff Duration `yaml:"initial_backoff"`
	MaxBackoff     Duration `yaml:"max_backoff"`
	MaxAttempts    int      `yaml:"max_attempts"`
	Window         Duration `yaml:"window"`
}

type ProxyConfig struct {
	Provider      string   `yaml:"provider"`
	Hosts         []string `yaml:"hosts"`
	AppRole       string   `yaml:"app_role"`
	SSL           string   `yaml:"ssl"`
	DeployTimeout Duration `yaml:"deploy_timeout"`
	DrainTimeout  Duration `yaml:"drain_timeout"`
}

type NetworkingConfig struct {
	PrivateNetwork string `yaml:"private_network"`
}

type AccessoryConfig struct {
	Image        string        `yaml:"image"`
	Hosts        []string      `yaml:"hosts"`
	Aliases      []string      `yaml:"aliases"`
	InternalPort int           `yaml:"internal_port"`
	Volumes      []string      `yaml:"volumes"`
	Restart      RestartConfig `yaml:"restart"`
}

type EnvConfig struct {
	Clear  map[string]string `yaml:"clear"`
	Secret []string          `yaml:"secret"`
}

type SecretsConfig struct {
	Provider string `yaml:"provider"`
	KMS      string `yaml:"kms"`
	Key      string `yaml:"key"`
}

type ObservabilityConfig struct {
	Logs          LogConfig           `yaml:"logs"`
	Metrics       MetricsConfig       `yaml:"metrics"`
	RuntimeEvents RuntimeEventsConfig `yaml:"runtime_events"`
}

type LogConfig struct {
	Format string `yaml:"format"`
	Level  string `yaml:"level"`
}

type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
}

type RuntimeEventsConfig struct {
	Enabled          bool `yaml:"enabled"`
	LogRestarts      bool `yaml:"log_restarts"`
	LogHealthChanges bool `yaml:"log_health_changes"`
	LogOOM           bool `yaml:"log_oom"`
}

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
var sshHostPattern = regexp.MustCompile(`^[A-Za-z0-9\[][A-Za-z0-9_.:@\[\]%-]*$`)

type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a scalar")
	}
	if value.Value == "" {
		d.Duration = 0
		return nil
	}

	parsed, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value.Value, err)
	}
	d.Duration = parsed
	return nil
}

type LoadOption func(*loadOptions)

type loadOptions struct {
	destination string
}

func WithDestination(destination string) LoadOption {
	return func(options *loadOptions) {
		options.destination = destination
	}
}

func Load(path string, opts ...LoadOption) (Config, error) {
	options := loadOptions{}
	for _, opt := range opts {
		opt(&options)
	}

	baseBytes, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("config file not found: %s", path)
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}

	merged, err := decodeMap(path, baseBytes)
	if err != nil {
		return Config{}, err
	}

	destination := options.destination
	if destination == "" {
		destination, _ = merged["destination"].(string)
	}
	if destination != "" {
		overlayPath := overlayPath(path, destination)
		overlayBytes, err := os.ReadFile(overlayPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("read destination overlay %q: %w", overlayPath, err)
		}
		if err == nil {
			overlay, err := decodeMap(overlayPath, overlayBytes)
			if err != nil {
				return Config{}, err
			}
			merged = deepMerge(merged, overlay)
		}
	}

	mergedBytes, err := yaml.Marshal(merged)
	if err != nil {
		return Config{}, fmt.Errorf("encode merged config: %w", err)
	}

	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(mergedBytes))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}

	applyDefaults(&cfg)
	if err := validate(cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func decodeMap(path string, data []byte) (map[string]any, error) {
	decoded := map[string]any{}
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	return decoded, nil
}

func overlayPath(path string, destination string) string {
	extension := filepath.Ext(path)
	name := strings.TrimSuffix(filepath.Base(path), extension)
	return filepath.Join(filepath.Dir(path), name+"."+destination+extension)
}

func deepMerge(base map[string]any, overlay map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(overlay))
	for key, value := range base {
		merged[key] = value
	}

	for key, overlayValue := range overlay {
		baseMap, baseOK := merged[key].(map[string]any)
		overlayMap, overlayOK := overlayValue.(map[string]any)
		if baseOK && overlayOK {
			merged[key] = deepMerge(baseMap, overlayMap)
			continue
		}
		merged[key] = overlayValue
	}

	return merged
}

func applyDefaults(cfg *Config) {
	if cfg.Networking.PrivateNetwork == "" {
		cfg.Networking.PrivateNetwork = "serve"
	}
	if cfg.RetainContainers == 0 {
		cfg.RetainContainers = 5
	}

	for name, server := range cfg.Servers {
		applyRestartDefaults(&server.Restart)
		cfg.Servers[name] = server
	}
	for name, accessory := range cfg.Accessories {
		applyRestartDefaults(&accessory.Restart)
		cfg.Accessories[name] = accessory
	}
}

func applyRestartDefaults(restart *RestartConfig) {
	if restart.Controller == "" {
		restart.Controller = "agent"
	}
}

func validate(cfg Config) error {
	var problems []string
	if strings.TrimSpace(cfg.Service) == "" {
		problems = append(problems, "service is required")
	} else if !validIdentifier(cfg.Service) {
		problems = append(problems, "service must contain only letters, numbers, dots, underscores, and hyphens")
	}
	if strings.TrimSpace(cfg.Image) == "" {
		problems = append(problems, "image is required")
	}
	if cfg.Destination != "" && !validIdentifier(cfg.Destination) {
		problems = append(problems, "destination must contain only letters, numbers, dots, underscores, and hyphens")
	}
	if cfg.Networking.PrivateNetwork != "" && !validIdentifier(cfg.Networking.PrivateNetwork) {
		problems = append(problems, "networking.private_network must be a valid identifier")
	}
	if cfg.RetainContainers < 0 {
		problems = append(problems, "retain_containers must not be negative")
	}
	for role, server := range cfg.Servers {
		if !validIdentifier(role) {
			problems = append(problems, fmt.Sprintf("servers.%s must use a valid identifier", role))
		}
		problems = append(problems, validateRestart("servers."+role+".restart", server.Restart)...)
		problems = append(problems, validateHealthcheck("servers."+role+".healthcheck", server.Healthcheck)...)
		problems = append(problems, validateSSHHosts("servers."+role+".hosts", server.Hosts)...)
		if server.Replicas < 0 {
			problems = append(problems, "servers."+role+".replicas must not be negative")
		}
	}
	for name, accessory := range cfg.Accessories {
		if !validIdentifier(name) {
			problems = append(problems, fmt.Sprintf("accessories.%s must use a valid identifier", name))
		}
		problems = append(problems, validateRestart("accessories."+name+".restart", accessory.Restart)...)
		problems = append(problems, validateSSHHosts("accessories."+name+".hosts", accessory.Hosts)...)
		if strings.TrimSpace(accessory.Image) == "" {
			problems = append(problems, "accessories."+name+".image is required")
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid config: %s", strings.Join(problems, "; "))
	}
	return nil
}

func ValidateSSHHost(host string) error {
	if !sshHostPattern.MatchString(host) {
		return fmt.Errorf("invalid SSH host %q", host)
	}
	return nil
}

func validateSSHHosts(path string, hosts []string) []string {
	var problems []string
	for _, host := range hosts {
		if err := ValidateSSHHost(host); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", path, err))
		}
	}
	return problems
}

func validIdentifier(value string) bool {
	return identifierPattern.MatchString(value)
}

func validateHealthcheck(path string, check HealthcheckConfig) []string {
	var problems []string
	if check.Interval.Duration < 0 {
		problems = append(problems, path+".interval must be positive")
	}
	if check.Timeout.Duration < 0 {
		problems = append(problems, path+".timeout must be positive")
	}
	if check.Retries < 0 {
		problems = append(problems, path+".retries must not be negative")
	}
	return problems
}

func validateRestart(path string, restart RestartConfig) []string {
	var problems []string
	if !validRestartPolicy(restart.Policy) {
		problems = append(problems, fmt.Sprintf("%s.policy must be one of always, unless-stopped, on-failure, no", path))
	}
	if !validRestartController(restart.Controller) {
		problems = append(problems, fmt.Sprintf("%s.controller must be one of agent, docker", path))
	}
	if restart.InitialBackoff.Duration < 0 {
		problems = append(problems, path+".initial_backoff must be positive")
	}
	if restart.MaxBackoff.Duration < 0 {
		problems = append(problems, path+".max_backoff must be positive")
	}
	if restart.MaxAttempts < 0 {
		problems = append(problems, path+".max_attempts must not be negative")
	}
	if restart.Window.Duration < 0 {
		problems = append(problems, path+".window must be positive")
	}
	if restart.Controller == "docker" {
		if restart.InitialBackoff.Duration > 0 {
			problems = append(problems, path+".initial_backoff is only supported by the agent restart controller")
		}
		if restart.MaxBackoff.Duration > 0 {
			problems = append(problems, path+".max_backoff is only supported by the agent restart controller")
		}
		if restart.Window.Duration > 0 {
			problems = append(problems, path+".window is only supported by the agent restart controller")
		}
		if restart.MaxAttempts > 0 && restart.Policy != "on-failure" {
			problems = append(problems, path+".max_attempts requires policy on-failure with the docker restart controller")
		}
	}
	return problems
}

func validRestartPolicy(policy string) bool {
	switch policy {
	case "", "always", "unless-stopped", "on-failure", "no":
		return true
	default:
		return false
	}
}

func validRestartController(controller string) bool {
	switch controller {
	case "agent", "docker":
		return true
	default:
		return false
	}
}
