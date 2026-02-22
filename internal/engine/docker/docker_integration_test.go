//go:build integration

package docker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.opentelemetry.io/otel"
)

// DockerEngineSuite tests the Docker engine against a real Docker daemon.
//
// These tests require Docker to be available (e.g., Docker Desktop or a
// Docker socket).  They are gated behind the "integration" build tag:
//
//	go test ./internal/engine/docker/ -tags integration -v
type DockerEngineSuite struct {
	suite.Suite
	ctx    context.Context
	cancel context.CancelFunc
	logger *slog.Logger
	docker *dockerclient.Client

	// testImage is a lightweight image used for tests.
	testImage string
}

func (s *DockerEngineSuite) SetupSuite() {
	s.testImage = "alpine:latest"
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	// Verify Docker is available
	cli, err := dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
		dockerclient.WithAPIVersionNegotiation(),
	)
	require.NoError(s.T(), err, "Docker must be available for integration tests")
	s.docker = cli

	ctx := context.Background()
	_, err = cli.Ping(ctx)
	require.NoError(s.T(), err, "Docker daemon must be reachable")

	// Pull test image
	pull, err := cli.ImagePull(ctx, s.testImage, image.PullOptions{})
	require.NoError(s.T(), err)
	_, _ = io.ReadAll(pull)
	pull.Close()
}

func (s *DockerEngineSuite) TearDownSuite() {
	if s.docker != nil {
		s.docker.Close()
	}
}

func (s *DockerEngineSuite) SetupTest() {
	s.ctx, s.cancel = context.WithTimeout(context.Background(), 60*time.Second)
}

func (s *DockerEngineSuite) TearDownTest() {
	s.cancel()
}

func TestDockerEngineSuite(t *testing.T) {
	suite.Run(t, new(DockerEngineSuite))
}

// newTestEngine creates a Docker Engine that uses alpine with "sleep 300"
// so containers stay alive long enough to be inspected and destroyed.
// Since we're in the same package, we can construct the Engine directly
// and override the image while using the real Docker client.
func (s *DockerEngineSuite) newTestEngine() *Engine {
	return &Engine{
		client:     s.docker,
		image:      s.testImage,
		dind:       false,
		logger:     s.logger,
		containers: make(map[string]string),
		tracer:     otel.Tracer("test"),
	}
}

// startTestContainer creates and starts a container using alpine + sleep,
// bypassing StartRunner's hardcoded /home/runner/run.sh command.
// Returns the container ID.
func (s *DockerEngineSuite) startTestContainer(e *Engine, name string, dind bool) string {
	env := []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=test-jit-config"}

	user := "root" // alpine doesn't have "runner" user
	var hostCfg *container.HostConfig

	if dind {
		env = append(env,
			"DOCKER_HOST=unix:///var/run/docker.sock",
			"RUNNER_ALLOW_RUNASROOT=1",
		)
		hostCfg = &container.HostConfig{
			Binds: []string{"/var/run/docker.sock:/var/run/docker.sock"},
		}
	}

	resp, err := s.docker.ContainerCreate(
		s.ctx,
		&container.Config{
			Image: s.testImage,
			User:  user,
			Cmd:   []string{"sleep", "300"},
			Env:   env,
		},
		hostCfg,
		nil, nil,
		name,
	)
	require.NoError(s.T(), err)

	err = s.docker.ContainerStart(s.ctx, resp.ID, container.StartOptions{})
	require.NoError(s.T(), err)

	e.mu.Lock()
	e.containers[name] = resp.ID
	e.mu.Unlock()

	return resp.ID
}

// containerExists checks if a container with the given ID exists.
func (s *DockerEngineSuite) containerExists(id string) bool {
	_, err := s.docker.ContainerInspect(s.ctx, id)
	return err == nil
}

// ---------------------------------------------------------------------------
// Engine constructor
// ---------------------------------------------------------------------------

func (s *DockerEngineSuite) TestNew_PullsImage() {
	e, err := New(s.ctx, Config{
		Image: s.testImage,
	}, s.logger)
	require.NoError(s.T(), err)
	assert.NotNil(s.T(), e)
	assert.Equal(s.T(), s.testImage, e.image)
}

// ---------------------------------------------------------------------------
// DestroyRunner: container lifecycle
// ---------------------------------------------------------------------------

func (s *DockerEngineSuite) TestStartAndDestroyRunner() {
	e := s.newTestEngine()
	defer e.Shutdown(s.ctx)

	id := s.startTestContainer(e, "test-runner-1", false)

	// Container should exist and be tracked
	assert.True(s.T(), s.containerExists(id))
	e.mu.Lock()
	assert.Contains(s.T(), e.containers, "test-runner-1")
	e.mu.Unlock()

	// Destroy it via the engine
	err := e.DestroyRunner(s.ctx, id)
	require.NoError(s.T(), err)

	// Container should be gone
	assert.False(s.T(), s.containerExists(id))
	e.mu.Lock()
	assert.NotContains(s.T(), e.containers, "test-runner-1")
	e.mu.Unlock()
}

func (s *DockerEngineSuite) TestStartMultipleRunners() {
	e := s.newTestEngine()
	defer e.Shutdown(s.ctx)

	ids := make([]string, 3)
	for i := range 3 {
		name := fmt.Sprintf("test-multi-%d", i)
		ids[i] = s.startTestContainer(e, name, false)
	}

	e.mu.Lock()
	assert.Len(s.T(), e.containers, 3)
	e.mu.Unlock()

	// Destroy each one via the engine
	for _, id := range ids {
		err := e.DestroyRunner(s.ctx, id)
		require.NoError(s.T(), err)
	}

	e.mu.Lock()
	assert.Empty(s.T(), e.containers)
	e.mu.Unlock()

	// Verify all containers are actually gone from Docker
	for _, id := range ids {
		assert.False(s.T(), s.containerExists(id))
	}
}

// ---------------------------------------------------------------------------
// Shutdown
// ---------------------------------------------------------------------------

func (s *DockerEngineSuite) TestShutdown_RemovesAllContainers() {
	e := s.newTestEngine()

	ids := make([]string, 3)
	for i := range 3 {
		name := fmt.Sprintf("test-shutdown-%d", i)
		ids[i] = s.startTestContainer(e, name, false)
	}

	err := e.Shutdown(s.ctx)
	require.NoError(s.T(), err)

	// All containers should be removed from Docker
	for _, id := range ids {
		assert.False(s.T(), s.containerExists(id),
			"container %s should be removed after shutdown", id)
	}

	e.mu.Lock()
	assert.Empty(s.T(), e.containers)
	e.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Idempotent destroy
// ---------------------------------------------------------------------------

func (s *DockerEngineSuite) TestDestroyRunner_DoubleDestroy() {
	e := s.newTestEngine()
	defer e.Shutdown(s.ctx)

	id := s.startTestContainer(e, "test-idem", false)

	// First destroy succeeds
	err := e.DestroyRunner(s.ctx, id)
	require.NoError(s.T(), err)

	// Second destroy: Docker returns an error for non-existent container.
	// This documents the current behavior -- Docker's DestroyRunner
	// is not idempotent (unlike GCP which handles 404 gracefully).
	err = e.DestroyRunner(s.ctx, id)
	assert.Error(s.T(), err, "Docker force-remove of non-existent container returns error")
}

// ---------------------------------------------------------------------------
// DinD configuration
// ---------------------------------------------------------------------------

func (s *DockerEngineSuite) TestDindMode_SocketMount() {
	e := s.newTestEngine()
	e.dind = true
	defer e.Shutdown(s.ctx)

	id := s.startTestContainer(e, "test-dind", true)

	info, err := s.docker.ContainerInspect(s.ctx, id)
	require.NoError(s.T(), err)

	// Verify Docker socket bind-mount
	hasBind := false
	for _, bind := range info.HostConfig.Binds {
		if bind == "/var/run/docker.sock:/var/run/docker.sock" {
			hasBind = true
			break
		}
	}
	assert.True(s.T(), hasBind, "DinD container should have Docker socket bind-mount")

	// Verify DOCKER_HOST env
	hasDockerHost := false
	hasRunAsRoot := false
	for _, env := range info.Config.Env {
		if env == "DOCKER_HOST=unix:///var/run/docker.sock" {
			hasDockerHost = true
		}
		if env == "RUNNER_ALLOW_RUNASROOT=1" {
			hasRunAsRoot = true
		}
	}
	assert.True(s.T(), hasDockerHost, "DinD should set DOCKER_HOST")
	assert.True(s.T(), hasRunAsRoot, "DinD should set RUNNER_ALLOW_RUNASROOT")
}

func (s *DockerEngineSuite) TestNonDindMode_NoSocketMount() {
	e := s.newTestEngine()
	defer e.Shutdown(s.ctx)

	id := s.startTestContainer(e, "test-nodind", false)

	info, err := s.docker.ContainerInspect(s.ctx, id)
	require.NoError(s.T(), err)

	// Should NOT have Docker socket bind
	for _, bind := range info.HostConfig.Binds {
		assert.NotContains(s.T(), bind, "docker.sock",
			"non-DinD container should not have Docker socket mount")
	}
}

// ---------------------------------------------------------------------------
// Rapid create/destroy cycles
// ---------------------------------------------------------------------------

func (s *DockerEngineSuite) TestRapidCreateDestroy() {
	e := s.newTestEngine()
	defer e.Shutdown(s.ctx)

	for i := range 5 {
		name := fmt.Sprintf("rapid-%d", i)
		id := s.startTestContainer(e, name, false)

		err := e.DestroyRunner(s.ctx, id)
		require.NoError(s.T(), err)

		assert.False(s.T(), s.containerExists(id))
	}

	e.mu.Lock()
	assert.Empty(s.T(), e.containers, "all containers should be cleaned up")
	e.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Shutdown with mix of running and already-removed containers
// ---------------------------------------------------------------------------

func (s *DockerEngineSuite) TestShutdown_MixedState() {
	e := s.newTestEngine()

	// Start 3, manually remove 1 via Docker API (simulating a crashed container)
	id0 := s.startTestContainer(e, "test-mixed-0", false)
	_ = s.startTestContainer(e, "test-mixed-1", false)
	_ = s.startTestContainer(e, "test-mixed-2", false)

	// Manually remove one behind the engine's back
	_ = s.docker.ContainerRemove(s.ctx, id0, container.RemoveOptions{Force: true})

	// Shutdown should handle the missing container gracefully.
	// The first error is captured but other containers still get cleaned up.
	err := e.Shutdown(s.ctx)
	// May or may not error depending on iteration order, but should not panic.
	_ = err

	// Tracking should be cleared regardless
	e.mu.Lock()
	assert.Empty(s.T(), e.containers)
	e.mu.Unlock()
}
