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
	GitHub   GitHubConfig   `yaml:"github"`
	ScaleSet ScaleSetConfig `yaml:"scaleset"`
	Engine   EngineConfig   `yaml:"engine"`
	Logging  LoggingConfig  `yaml:"logging"`
	OTel     OTelConfig     `yaml:"otel"`
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
type EngineConfig struct {
	// Type selects the compute backend: "docker", "gcp" (future: "ec2", "azure").
	Type string `yaml:"type"`

	// Docker holds Docker-specific settings.  Only read when Type == "docker".
	Docker DockerEngineConfig `yaml:"docker"`

	// GCP holds GCP Compute Engine settings.  Only read when Type == "gcp".
	GCP GCPEngineConfig `yaml:"gcp"`
}

// DockerEngineConfig holds Docker-specific engine settings.
type DockerEngineConfig struct {
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
	// Project is the GCP project ID (required when engine.type == "gcp").
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
	if c.Engine.Type == "" {
		c.Engine.Type = "docker"
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

	switch c.Engine.Type {
	case "docker":
		// OK
	case "gcp":
		if c.Engine.GCP.Project == "" {
			return fmt.Errorf("engine.gcp.project is required when engine.type is \"gcp\"")
		}
		if c.Engine.GCP.Zone == "" {
			return fmt.Errorf("engine.gcp.zone is required when engine.type is \"gcp\"")
		}
		if c.Engine.GCP.Image == "" {
			return fmt.Errorf("engine.gcp.image is required when engine.type is \"gcp\"")
		}
	default:
		return fmt.Errorf("engine.type %q is not supported (supported: docker, gcp)", c.Engine.Type)
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

// NewEngine creates the compute engine selected by engine.type.
func (c *Config) NewEngine(ctx context.Context, logger *slog.Logger) (engine.Engine, error) {
	switch c.Engine.Type {
	case "docker":
		return docker.New(ctx, docker.Config{
			Image: c.Engine.Docker.Image,
			Dind:  c.Engine.Docker.Dind,
		}, logger.WithGroup("engine.docker"))
	case "gcp":
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
	default:
		return nil, fmt.Errorf("unsupported engine type: %s", c.Engine.Type)
	}
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
