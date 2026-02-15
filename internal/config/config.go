// Package config handles loading, validating, and applying
// configuration for the scaleset runner.  Configuration is read from a
// YAML file and can be overridden by CLI flags.
package config

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/actions/scaleset"
	"gopkg.in/yaml.v3"

	"github.com/terrpan/scaleset/internal/engine"
	"github.com/terrpan/scaleset/internal/engine/docker"
	"github.com/terrpan/scaleset/internal/engine/gcp"
)

// ---------------------------------------------------------------------------
// Top-level config
// ---------------------------------------------------------------------------

// Config is the root configuration structure.
type Config struct {
	GitHub     GitHubConfig     `yaml:"github"`
	ScaleSet   ScaleSetConfig   `yaml:"scaleset"`
	Engine     EngineConfig     `yaml:"engine"`
	Logging    LoggingConfig    `yaml:"logging"`
	OTel       OTelConfig       `yaml:"otel"`
	Prometheus PrometheusConfig `yaml:"prometheus"`
}

// ---------------------------------------------------------------------------
// GitHub / auth
// ---------------------------------------------------------------------------

// GitHubConfig holds credentials and the registration URL.
type GitHubConfig struct {
	// URL is the full GitHub URL where the scale set is registered
	// (e.g. https://github.com/org/repo).
	URL string `yaml:"url"`

	// App holds GitHub App credentials (recommended).
	App GitHubAppConfig `yaml:"app"`

	// Token is a personal access token (alternative to App).
	Token string `yaml:"token"`
}

// GitHubAppConfig mirrors scaleset.GitHubAppAuth but adds a
// PrivateKeyPath field so the key can live in a file.
type GitHubAppConfig struct {
	ClientID       string `yaml:"client_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
	// PrivateKey can be set directly (e.g. via CLI flag).  If both
	// PrivateKeyPath and PrivateKey are set, PrivateKey wins.
	PrivateKey string `yaml:"private_key"`
}

// ---------------------------------------------------------------------------
// Scale set
// ---------------------------------------------------------------------------

// ScaleSetConfig describes the runner scale set to create.
type ScaleSetConfig struct {
	Name        string   `yaml:"name"`
	Labels      []string `yaml:"labels"`
	RunnerGroup string   `yaml:"runner_group"`
	MinRunners  int      `yaml:"min_runners"`
	MaxRunners  int      `yaml:"max_runners"`
}

// ---------------------------------------------------------------------------
// Engine
// ---------------------------------------------------------------------------

// EngineConfig selects and configures the compute backend.
// Exactly one engine must have Enable set to true.
type EngineConfig struct {
	// Docker holds Docker-specific settings.
	Docker DockerEngineConfig `yaml:"docker"`

	// GCP holds GCP Compute Engine settings.
	GCP GCPEngineConfig `yaml:"gcp"`

	// AWS holds AWS EC2 settings (not yet implemented).
	AWS AWSEngineConfig `yaml:"aws"`

	// Azure holds Azure VM settings (not yet implemented).
	Azure AzureEngineConfig `yaml:"azure"`
}

// DockerEngineConfig holds Docker-specific engine settings.
type DockerEngineConfig struct {
	// Enable activates the Docker engine.
	Enable bool `yaml:"enable"`
	// Image is the container image for the runner.  Use ":latest" (default) for
	// the newest release, or pin a specific version (e.g. "ghcr.io/actions/actions-runner:2.323.0").
	// Default: "ghcr.io/actions/actions-runner:latest"
	Image string `yaml:"image"`
	// Dind enables Docker-in-Docker by bind-mounting the host's
	// Docker socket into each runner container.
	Dind bool `yaml:"dind"`
}

// GCPEngineConfig holds GCP Compute Engine engine settings.
//
// Authentication uses Application Default Credentials (ADC) -- no
// credential fields are needed.  See docs/gcp/README.md for details.
type GCPEngineConfig struct {
	// Enable activates the GCP engine.
	Enable bool `yaml:"enable"`

	// Project is the GCP project ID (required when GCP is enabled).
	Project string `yaml:"project"`

	// Zone is the GCP zone for runner VMs (required).
	Zone string `yaml:"zone"`

	// MachineType is the Compute Engine machine type.  Default: "e2-medium".
	MachineType string `yaml:"machine_type"`

	// Image is the full self-link or family URL of the runner image (required).
	// Examples:
	//   "projects/my-project/global/images/scaleset-runner-1234567890"
	//   "projects/my-project/global/images/family/scaleset-runner"
	Image string `yaml:"image"`

	// DiskSizeGB is the boot disk size in GB.  Default: 50.
	DiskSizeGB int64 `yaml:"disk_size_gb"`

	// Network is the VPC network name.  Default: "default".
	Network string `yaml:"network"`

	// Subnet is the subnetwork (optional).  If empty, the default
	// subnet for the zone is used.
	Subnet string `yaml:"subnet"`

	// PublicIP controls whether runner VMs get an external IP address.
	// Default: true.  Use a *bool so we can distinguish "not set"
	// (nil -> default true) from "explicitly set to false".
	PublicIP *bool `yaml:"public_ip"`

	// ServiceAccount is the GCP service account email to attach to
	// runner VMs (optional).
	ServiceAccount string `yaml:"service_account"`
}

// AWSEngineConfig holds AWS EC2 engine settings (not yet implemented).
type AWSEngineConfig struct {
	// Enable activates the AWS engine.
	Enable bool `yaml:"enable"`

	// Region is the AWS region (e.g. "us-east-1").
	Region string `yaml:"region"`

	// Image is the AMI ID (e.g. "ami-0c55b159cbfafe1f0").
	Image string `yaml:"image"`

	// InstanceType is the EC2 instance type (e.g. "t3.medium").
	InstanceType string `yaml:"instance_type"`

	// DiskSizeGB is the root volume size in GB.  Default: 50.
	DiskSizeGB int64 `yaml:"disk_size_gb"`
}

// AzureEngineConfig holds Azure VM engine settings (not yet implemented).
type AzureEngineConfig struct {
	// Enable activates the Azure engine.
	Enable bool `yaml:"enable"`

	// SubscriptionID is the Azure subscription ID.
	SubscriptionID string `yaml:"subscription_id"`

	// ResourceGroup is the Azure resource group name.
	ResourceGroup string `yaml:"resource_group"`

	// Image is the Azure image reference (e.g. "MicrosoftWindowsServer:WindowsServer:2019-Datacenter:latest").
	Image string `yaml:"image"`

	// VMSize is the Azure VM size (e.g. "Standard_DS2_v2").
	VMSize string `yaml:"vm_size"`

	// DiskSizeGB is the OS disk size in GB.  Default: 50.
	DiskSizeGB int64 `yaml:"disk_size_gb"`
}

// EnabledEngine returns the name of the enabled engine ("docker", "gcp", "aws", or "azure"),
// or an empty string if no engine is enabled.
func (e *EngineConfig) EnabledEngine() string {
	if e.Docker.Enable {
		return "docker"
	}
	if e.GCP.Enable {
		return "gcp"
	}
	if e.AWS.Enable {
		return "aws"
	}
	if e.Azure.Enable {
		return "azure"
	}
	return ""
}

// ---------------------------------------------------------------------------
// Logging
// ---------------------------------------------------------------------------

// LoggingConfig controls structured logging output.
type LoggingConfig struct {
	// Level: debug, info, warn, error.  Default: info.
	Level string `yaml:"level"`
	// Format: text, json.  Default: text.
	Format string `yaml:"format"`
}

// ---------------------------------------------------------------------------
// OpenTelemetry
// ---------------------------------------------------------------------------

// OTelConfig controls OpenTelemetry tracing and metrics.
type OTelConfig struct {
	// Enabled controls whether OpenTelemetry is active.  Default: false.
	Enabled bool `yaml:"enabled"`

	// Endpoint is the OTLP HTTP endpoint (e.g. "localhost:4318").
	// If empty, falls back to OTEL_EXPORTER_OTLP_ENDPOINT env var.
	// Default: "" (uses OTEL env vars).
	Endpoint string `yaml:"endpoint"`

	// Insecure enables plain HTTP (no TLS) for OTLP export.  Default: true.
	Insecure bool `yaml:"insecure"`

	// StdOut also prints traces and metrics to stdout (for debugging).  Default: false.
	StdOut bool `yaml:"stdout"`
}

// ---------------------------------------------------------------------------
// Prometheus
// ---------------------------------------------------------------------------

// PrometheusConfig controls the Prometheus /metrics scrape endpoint.
// This works independently of the OTel section -- you can use Prometheus
// without OTLP tracing, or both together.
type PrometheusConfig struct {
	// Enable activates the Prometheus /metrics HTTP endpoint.  Default: false.
	Enable bool `yaml:"enable"`
	// Port is the HTTP port for the /metrics endpoint.  Default: 9090.
	Port int `yaml:"port"`
}

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

// Load reads a YAML config file from path and returns the parsed Config.
// If the file does not exist the returned Config will contain zero values
// which must be filled via flag overrides before calling Validate.
func Load(path string) (*Config, error) {
	cfg := &Config{}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Config file is optional -- flags can supply everything.
			return cfg, nil
		}
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	return cfg, nil
}

// ---------------------------------------------------------------------------
// Defaults & validation
// ---------------------------------------------------------------------------

// ApplyDefaults fills in sensible defaults for any unset fields.
func (c *Config) ApplyDefaults() {
	if c.ScaleSet.RunnerGroup == "" {
		c.ScaleSet.RunnerGroup = scaleset.DefaultRunnerGroup
	}
	if c.ScaleSet.MaxRunners == 0 {
		c.ScaleSet.MaxRunners = 10
	}
	if c.Engine.Docker.Image == "" {
		c.Engine.Docker.Image = "ghcr.io/actions/actions-runner:latest"
	}
	if c.Engine.GCP.MachineType == "" {
		c.Engine.GCP.MachineType = "e2-medium"
	}
	if c.Engine.GCP.DiskSizeGB == 0 {
		c.Engine.GCP.DiskSizeGB = 50
	}
	if c.Engine.GCP.PublicIP == nil {
		t := true
		c.Engine.GCP.PublicIP = &t
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "text"
	}
	// OTel defaults: disabled by default, insecure=true for local dev
	if !c.OTel.Enabled {
		// If explicitly disabled, ensure insecure defaults to true for when enabled
		if c.OTel.Insecure == false && c.OTel.Endpoint == "" {
			c.OTel.Insecure = true
		}
	}
	// Prometheus defaults
	if c.Prometheus.Port == 0 {
		c.Prometheus.Port = 9090
	}
}

// Validate checks that all required fields are present and consistent.
func (c *Config) Validate() error {
	c.ApplyDefaults()

	if _, err := url.ParseRequestURI(c.GitHub.URL); err != nil {
		return fmt.Errorf("github.url: invalid URL %q: %w", c.GitHub.URL, err)
	}

	if err := c.validateAuth(); err != nil {
		return err
	}

	if c.ScaleSet.Name == "" {
		return fmt.Errorf("scaleset.name is required")
	}
	for i, l := range c.ScaleSet.Labels {
		if strings.TrimSpace(l) == "" {
			return fmt.Errorf("scaleset.labels[%d] is empty", i)
		}
	}
	if c.ScaleSet.MaxRunners < c.ScaleSet.MinRunners {
		return fmt.Errorf("scaleset.max_runners (%d) < scaleset.min_runners (%d)", c.ScaleSet.MaxRunners, c.ScaleSet.MinRunners)
	}

	// Validate exactly one engine is enabled
	enabled := []string{}
	if c.Engine.Docker.Enable {
		enabled = append(enabled, "docker")
	}
	if c.Engine.GCP.Enable {
		enabled = append(enabled, "gcp")
	}
	if c.Engine.AWS.Enable {
		enabled = append(enabled, "aws")
	}
	if c.Engine.Azure.Enable {
		enabled = append(enabled, "azure")
	}

	if len(enabled) == 0 {
		return fmt.Errorf("at least one engine must have enable: true (supported: docker, gcp; planned: aws, azure)")
	}
	if len(enabled) > 1 {
		return fmt.Errorf("only one engine can be enabled at a time, but %d are enabled: %v", len(enabled), enabled)
	}

	// Validate the enabled engine's required fields
	switch enabled[0] {
	case "docker":
		// No required fields for Docker
	case "gcp":
		if c.Engine.GCP.Project == "" {
			return fmt.Errorf("engine.gcp.project is required when GCP engine is enabled")
		}
		if c.Engine.GCP.Zone == "" {
			return fmt.Errorf("engine.gcp.zone is required when GCP engine is enabled")
		}
		if c.Engine.GCP.Image == "" {
			return fmt.Errorf("engine.gcp.image is required when GCP engine is enabled")
		}
	case "aws":
		return fmt.Errorf("aws engine is not yet implemented")
	case "azure":
		return fmt.Errorf("azure engine is not yet implemented")
	}

	return nil
}

func (c *Config) validateAuth() error {
	hasToken := c.GitHub.Token != ""
	hasApp := c.GitHub.App.ClientID != "" ||
		c.GitHub.App.InstallationID != 0 ||
		c.GitHub.App.PrivateKey != "" ||
		c.GitHub.App.PrivateKeyPath != ""

	if !hasToken && !hasApp {
		return fmt.Errorf("no credentials: provide github.app (recommended) or github.token")
	}

	if hasApp {
		if c.GitHub.App.ClientID == "" {
			return fmt.Errorf("github.app.client_id is required when using GitHub App auth")
		}
		if c.GitHub.App.InstallationID == 0 {
			return fmt.Errorf("github.app.installation_id is required when using GitHub App auth")
		}
		if c.GitHub.App.PrivateKey == "" && c.GitHub.App.PrivateKeyPath == "" {
			return fmt.Errorf("github.app.private_key or github.app.private_key_path is required")
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Factories
// ---------------------------------------------------------------------------

// NewLogger creates a *slog.Logger from the Logging configuration.
func (c *Config) NewLogger() *slog.Logger {
	opts := &slog.HandlerOptions{
		AddSource: true,
		Level:     c.slogLevel(),
	}

	switch strings.ToLower(c.Logging.Format) {
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	case "text":
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	default:
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
}

func (c *Config) slogLevel() slog.Level {
	switch strings.ToLower(c.Logging.Level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// NewScalesetClient creates a scaleset.Client using the configured
// credentials (GitHub App or PAT).
func (c *Config) NewScalesetClient() (*scaleset.Client, error) {
	if err := c.resolvePrivateKey(); err != nil {
		return nil, err
	}

	sysInfo := scaleset.SystemInfo{
		System:    "terrpan-scaleset",
		Subsystem: "cli",
		Version:   "0.1.0",
		CommitSHA: "dev",
	}

	if c.GitHub.App.ClientID != "" {
		return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
			GitHubConfigURL: c.GitHub.URL,
			GitHubAppAuth: scaleset.GitHubAppAuth{
				ClientID:       c.GitHub.App.ClientID,
				InstallationID: c.GitHub.App.InstallationID,
				PrivateKey:     c.GitHub.App.PrivateKey,
			},
			SystemInfo: sysInfo,
		})
	}

	return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL:     c.GitHub.URL,
		PersonalAccessToken: c.GitHub.Token,
		SystemInfo:          sysInfo,
	})
}

// resolvePrivateKey reads the private key from PrivateKeyPath if
// PrivateKey is not already set.
func (c *Config) resolvePrivateKey() error {
	if c.GitHub.App.PrivateKey != "" || c.GitHub.App.PrivateKeyPath == "" {
		return nil
	}
	data, err := os.ReadFile(c.GitHub.App.PrivateKeyPath)
	if err != nil {
		return fmt.Errorf("reading private key from %s: %w", c.GitHub.App.PrivateKeyPath, err)
	}
	c.GitHub.App.PrivateKey = string(data)
	return nil
}

// NewEngine creates the compute engine based on which engine is enabled.
func (c *Config) NewEngine(ctx context.Context, logger *slog.Logger) (engine.Engine, error) {
	if c.Engine.Docker.Enable {
		return docker.New(ctx, docker.Config{
			Image: c.Engine.Docker.Image,
			Dind:  c.Engine.Docker.Dind,
		}, logger.WithGroup("engine.docker"))
	}
	if c.Engine.GCP.Enable {
		return gcp.New(ctx, gcp.Config{
			Project:        c.Engine.GCP.Project,
			Zone:           c.Engine.GCP.Zone,
			MachineType:    c.Engine.GCP.MachineType,
			Image:          c.Engine.GCP.Image,
			DiskSizeGB:     c.Engine.GCP.DiskSizeGB,
			Network:        c.Engine.GCP.Network,
			Subnet:         c.Engine.GCP.Subnet,
			PublicIP:       *c.Engine.GCP.PublicIP,
			ServiceAccount: c.Engine.GCP.ServiceAccount,
		}, logger.WithGroup("engine.gcp"))
	}
	if c.Engine.AWS.Enable {
		return nil, fmt.Errorf("aws engine is not yet implemented")
	}
	if c.Engine.Azure.Enable {
		return nil, fmt.Errorf("azure engine is not yet implemented")
	}

	return nil, fmt.Errorf("no engine is enabled")
}

// BuildLabels returns scaleset.Label values from the configured labels.
// If no labels are configured, the scale set name is used as the label.
func (c *Config) BuildLabels() []scaleset.Label {
	if len(c.ScaleSet.Labels) > 0 {
		labels := make([]scaleset.Label, len(c.ScaleSet.Labels))
		for i, name := range c.ScaleSet.Labels {
			labels[i] = scaleset.Label{Name: strings.TrimSpace(name)}
		}
		return labels
	}
	return []scaleset.Label{{Name: c.ScaleSet.Name}}
}
