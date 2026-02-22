package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// validDockerConfig returns a minimal Config that passes Validate() with
// Docker engine enabled and PAT auth.
func validDockerConfig() *Config {
	return &Config{
		GitHub: GitHubConfig{
			URL:   "https://github.com/my-org/my-repo",
			Token: "ghp_test_token",
		},
		ScaleSet: ScaleSetConfig{
			Name:       "test-scaleset",
			MaxRunners: 10,
		},
		Engine: EngineConfig{
			Docker: DockerEngineConfig{Enable: true},
		},
	}
}

// validGCPConfig returns a minimal Config that passes Validate() with
// GCP engine enabled and PAT auth.
func validGCPConfig() *Config {
	return &Config{
		GitHub: GitHubConfig{
			URL:   "https://github.com/my-org/my-repo",
			Token: "ghp_test_token",
		},
		ScaleSet: ScaleSetConfig{
			Name:       "test-scaleset",
			MaxRunners: 10,
		},
		Engine: EngineConfig{
			GCP: GCPEngineConfig{
				Enable:  true,
				Project: "my-project",
				Zone:    "us-central1-a",
				Image:   "projects/my-project/global/images/runner",
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Test suite
// ---------------------------------------------------------------------------

type ConfigValidationSuite struct {
	suite.Suite
}

func TestConfigValidationSuite(t *testing.T) {
	suite.Run(t, new(ConfigValidationSuite))
}

// ---------------------------------------------------------------------------
// Valid configs
// ---------------------------------------------------------------------------

func (s *ConfigValidationSuite) TestValidate_ValidDockerConfig() {
	cfg := validDockerConfig()
	err := cfg.Validate()
	require.NoError(s.T(), err)
}

func (s *ConfigValidationSuite) TestValidate_ValidGCPConfig() {
	cfg := validGCPConfig()
	err := cfg.Validate()
	require.NoError(s.T(), err)
}

func (s *ConfigValidationSuite) TestValidate_ValidAppAuth() {
	cfg := validDockerConfig()
	cfg.GitHub.Token = ""
	cfg.GitHub.App = GitHubAppConfig{
		ClientID:       "Iv1.abc123",
		InstallationID: 12345,
		PrivateKey:     "-----BEGIN RSA PRIVATE KEY-----\nfake\n-----END RSA PRIVATE KEY-----",
	}
	err := cfg.Validate()
	require.NoError(s.T(), err)
}

// ---------------------------------------------------------------------------
// GitHub URL validation
// ---------------------------------------------------------------------------

func (s *ConfigValidationSuite) TestValidate_MissingURL() {
	cfg := validDockerConfig()
	cfg.GitHub.URL = ""
	err := cfg.Validate()
	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "github.url")
}

func (s *ConfigValidationSuite) TestValidate_InvalidURL() {
	cfg := validDockerConfig()
	cfg.GitHub.URL = "not-a-url"
	err := cfg.Validate()
	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "github.url")
}

// ---------------------------------------------------------------------------
// Auth validation
// ---------------------------------------------------------------------------

func (s *ConfigValidationSuite) TestValidate_MissingAuth() {
	cfg := validDockerConfig()
	cfg.GitHub.Token = ""
	cfg.GitHub.App = GitHubAppConfig{}
	err := cfg.Validate()
	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "no credentials")
}

func (s *ConfigValidationSuite) TestValidate_AppAuth_MissingClientID() {
	cfg := validDockerConfig()
	cfg.GitHub.Token = ""
	cfg.GitHub.App = GitHubAppConfig{
		InstallationID: 12345,
		PrivateKey:     "key",
	}
	err := cfg.Validate()
	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "client_id")
}

func (s *ConfigValidationSuite) TestValidate_AppAuth_MissingInstallationID() {
	cfg := validDockerConfig()
	cfg.GitHub.Token = ""
	cfg.GitHub.App = GitHubAppConfig{
		ClientID:   "Iv1.abc123",
		PrivateKey: "key",
	}
	err := cfg.Validate()
	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "installation_id")
}

func (s *ConfigValidationSuite) TestValidate_AppAuth_MissingPrivateKey() {
	cfg := validDockerConfig()
	cfg.GitHub.Token = ""
	cfg.GitHub.App = GitHubAppConfig{
		ClientID:       "Iv1.abc123",
		InstallationID: 12345,
	}
	err := cfg.Validate()
	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "private_key")
}

// ---------------------------------------------------------------------------
// ScaleSet validation
// ---------------------------------------------------------------------------

func (s *ConfigValidationSuite) TestValidate_MissingScaleSetName() {
	cfg := validDockerConfig()
	cfg.ScaleSet.Name = ""
	err := cfg.Validate()
	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "scaleset.name")
}

func (s *ConfigValidationSuite) TestValidate_MaxLessThanMin() {
	cfg := validDockerConfig()
	cfg.ScaleSet.MinRunners = 10
	cfg.ScaleSet.MaxRunners = 5
	err := cfg.Validate()
	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "max_runners")
}

func (s *ConfigValidationSuite) TestValidate_EmptyLabel() {
	cfg := validDockerConfig()
	cfg.ScaleSet.Labels = []string{"good", "  ", "also-good"}
	err := cfg.Validate()
	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "labels")
}

// ---------------------------------------------------------------------------
// Engine validation
// ---------------------------------------------------------------------------

func (s *ConfigValidationSuite) TestValidate_NoEngineEnabled() {
	cfg := validDockerConfig()
	cfg.Engine.Docker.Enable = false
	err := cfg.Validate()
	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "engine")
}

func (s *ConfigValidationSuite) TestValidate_MultipleEnginesEnabled() {
	cfg := validDockerConfig()
	cfg.Engine.GCP = GCPEngineConfig{
		Enable:  true,
		Project: "p",
		Zone:    "z",
		Image:   "i",
	}
	err := cfg.Validate()
	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "only one engine")
}

func (s *ConfigValidationSuite) TestValidate_GCP_MissingProject() {
	cfg := validGCPConfig()
	cfg.Engine.GCP.Project = ""
	err := cfg.Validate()
	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "project")
}

func (s *ConfigValidationSuite) TestValidate_GCP_MissingZone() {
	cfg := validGCPConfig()
	cfg.Engine.GCP.Zone = ""
	err := cfg.Validate()
	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "zone")
}

func (s *ConfigValidationSuite) TestValidate_GCP_MissingImage() {
	cfg := validGCPConfig()
	cfg.Engine.GCP.Image = ""
	err := cfg.Validate()
	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "image")
}

func (s *ConfigValidationSuite) TestValidate_AWSNotImplemented() {
	cfg := validDockerConfig()
	cfg.Engine.Docker.Enable = false
	cfg.Engine.AWS = AWSEngineConfig{Enable: true}
	err := cfg.Validate()
	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "not yet implemented")
}

func (s *ConfigValidationSuite) TestValidate_AzureNotImplemented() {
	cfg := validDockerConfig()
	cfg.Engine.Docker.Enable = false
	cfg.Engine.Azure = AzureEngineConfig{Enable: true}
	err := cfg.Validate()
	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "not yet implemented")
}

// ---------------------------------------------------------------------------
// Defaults
// ---------------------------------------------------------------------------

func (s *ConfigValidationSuite) TestApplyDefaults_SetsExpectedValues() {
	cfg := &Config{}
	cfg.ApplyDefaults()

	assert.Equal(s.T(), 10, cfg.ScaleSet.MaxRunners)
	assert.Equal(s.T(), "ghcr.io/actions/actions-runner:latest", cfg.Engine.Docker.Image)
	assert.Equal(s.T(), "e2-medium", cfg.Engine.GCP.MachineType)
	assert.Equal(s.T(), int64(50), cfg.Engine.GCP.DiskSizeGB)
	assert.NotNil(s.T(), cfg.Engine.GCP.PublicIP)
	assert.True(s.T(), *cfg.Engine.GCP.PublicIP)
	assert.Equal(s.T(), "info", cfg.Logging.Level)
	assert.Equal(s.T(), "text", cfg.Logging.Format)
	assert.Equal(s.T(), 9090, cfg.Prometheus.Port)
}

// ---------------------------------------------------------------------------
// EnabledEngine helper
// ---------------------------------------------------------------------------

func (s *ConfigValidationSuite) TestEnabledEngine() {
	tests := []struct {
		name   string
		cfg    EngineConfig
		expect string
	}{
		{"docker", EngineConfig{Docker: DockerEngineConfig{Enable: true}}, "docker"},
		{"gcp", EngineConfig{GCP: GCPEngineConfig{Enable: true}}, "gcp"},
		{"aws", EngineConfig{AWS: AWSEngineConfig{Enable: true}}, "aws"},
		{"azure", EngineConfig{Azure: AzureEngineConfig{Enable: true}}, "azure"},
		{"none", EngineConfig{}, ""},
	}

	for _, tc := range tests {
		s.Run(tc.name, func() {
			assert.Equal(s.T(), tc.expect, tc.cfg.EnabledEngine())
		})
	}
}

// ---------------------------------------------------------------------------
// BuildLabels
// ---------------------------------------------------------------------------

func (s *ConfigValidationSuite) TestBuildLabels_WithLabels() {
	cfg := validDockerConfig()
	cfg.ScaleSet.Labels = []string{"linux", "x64", "gpu"}
	labels := cfg.BuildLabels()
	assert.Len(s.T(), labels, 3)
	assert.Equal(s.T(), "linux", labels[0].Name)
	assert.Equal(s.T(), "x64", labels[1].Name)
	assert.Equal(s.T(), "gpu", labels[2].Name)
}

func (s *ConfigValidationSuite) TestBuildLabels_FallsBackToName() {
	cfg := validDockerConfig()
	cfg.ScaleSet.Labels = nil
	labels := cfg.BuildLabels()
	assert.Len(s.T(), labels, 1)
	assert.Equal(s.T(), "test-scaleset", labels[0].Name)
}

func (s *ConfigValidationSuite) TestBuildLabels_TrimsWhitespace() {
	cfg := validDockerConfig()
	cfg.ScaleSet.Labels = []string{"  linux  ", "x64"}
	labels := cfg.BuildLabels()
	assert.Equal(s.T(), "linux", labels[0].Name)
}
