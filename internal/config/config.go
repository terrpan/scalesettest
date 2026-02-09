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
	// Type selects the compute backend: "docker", (future: "ec2", "azure", "gcp").
	Type string `yaml:"type"`

	// Docker holds Docker-specific settings.  Only read when Type == "docker".
	Docker DockerEngineConfig `yaml:"docker"`
}

// DockerEngineConfig holds Docker-specific engine settings.
type DockerEngineConfig struct {
	Image string `yaml:"image"`
	// Dind enables Docker-in-Docker by bind-mounting the host's
	// Docker socket into each runner container.
	Dind bool `yaml:"dind"`
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
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "text"
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
	default:
		return fmt.Errorf("engine.type %q is not supported (supported: docker)", c.Engine.Type)
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
