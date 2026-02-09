// Package docker implements the engine.Engine interface using the
// Docker daemon to run ephemeral GitHub Actions runners as containers.
package docker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"

	"github.com/terrpan/scaleset/internal/engine"
)

// Config holds Docker-specific settings.
type Config struct {
	// Image is the container image to use for runners.
	// Default: ghcr.io/actions/actions-runner:latest
	Image string

	// Dind enables Docker-in-Docker by bind-mounting the host's Docker
	// socket (/var/run/docker.sock) into each runner container.  This
	// allows workflows to run Docker commands (docker build, docker
	// compose, container actions, etc.).
	//
	// Security note: the socket gives the runner full access to the
	// host Docker daemon.  Only enable this if you trust the workflows
	// that will run on these runners.
	Dind bool
}

// Engine manages GitHub Actions runners as Docker containers.
type Engine struct {
	client *dockerclient.Client
	image  string
	dind   bool
	logger *slog.Logger

	mu         sync.Mutex
	containers map[string]string // name -> containerID
}

// Compile-time check that Engine satisfies the engine.Engine interface.
var _ engine.Engine = (*Engine)(nil)

// New creates a Docker engine, connects to the daemon, and pulls the
// runner image so it is available for container creation.
func New(ctx context.Context, cfg Config, logger *slog.Logger) (*Engine, error) {
	if cfg.Image == "" {
		cfg.Image = "ghcr.io/actions/actions-runner:latest"
	}

	client, err := dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}

	logger.Info("pulling runner image", slog.String("image", cfg.Image))

	pull, err := client.ImagePull(ctx, cfg.Image, image.PullOptions{})
	if err != nil {
		return nil, fmt.Errorf("image pull %s: %w", cfg.Image, err)
	}
	// Drain and close the pull stream so the image is fully downloaded.
	if _, err := io.ReadAll(pull); err != nil {
		return nil, fmt.Errorf("reading image pull response: %w", err)
	}
	if err := pull.Close(); err != nil {
		return nil, fmt.Errorf("closing image pull stream: %w", err)
	}

	logger.Info("runner image ready", slog.String("image", cfg.Image))

	return &Engine{
		client:     client,
		image:      cfg.Image,
		dind:       cfg.Dind,
		logger:     logger,
		containers: make(map[string]string),
	}, nil
}

// StartRunner creates and starts a Docker container that runs a
// GitHub Actions runner with the provided JIT configuration.
func (e *Engine) StartRunner(ctx context.Context, name string, jitConfig string) (string, error) {
	env := []string{
		fmt.Sprintf("ACTIONS_RUNNER_INPUT_JITCONFIG=%s", jitConfig),
	}

	// When DinD is enabled, run as root for cross-platform socket access.
	// On Linux, the docker group has write permission; on macOS Docker
	// Desktop, only the owner does.  Running as root works on both.
	user := "runner"
	if e.dind {
		user = "root"
	}

	var hostCfg *container.HostConfig
	if e.dind {
		env = append(env,
			"DOCKER_HOST=unix:///var/run/docker.sock",
			"RUNNER_ALLOW_RUNASROOT=1",
		)
		hostCfg = &container.HostConfig{
			Binds: []string{"/var/run/docker.sock:/var/run/docker.sock"},
		}
		e.logger.Info("dind enabled: mounting docker socket, running as root for cross-platform compatibility",
			slog.String("name", name),
		)
	}

	resp, err := e.client.ContainerCreate(
		ctx,
		&container.Config{
			Image: e.image,
			User:  user,
			Cmd:   []string{"/home/runner/run.sh"},
			Env:   env,
		},
		hostCfg,
		nil, // networking config
		nil, // platform
		name,
	)
	if err != nil {
		return "", fmt.Errorf("container create %s: %w", name, err)
	}

	if err := e.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// Best-effort cleanup of the created-but-not-started container.
		_ = e.client.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("container start %s: %w", name, err)
	}

	e.mu.Lock()
	e.containers[name] = resp.ID
	e.mu.Unlock()

	e.logger.Info("runner started",
		slog.String("name", name),
		slog.String("containerID", resp.ID),
	)

	return resp.ID, nil
}

// DestroyRunner force-removes the container identified by id,
// permanently destroying the ephemeral runner.
func (e *Engine) DestroyRunner(ctx context.Context, id string) error {
	e.logger.Info("destroying runner", slog.String("containerID", id))

	if err := e.client.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("container remove %s: %w", id, err)
	}

	// Remove from tracking map.
	e.mu.Lock()
	for name, cid := range e.containers {
		if cid == id {
			delete(e.containers, name)
			break
		}
	}
	e.mu.Unlock()

	return nil
}

// Shutdown force-removes every container this engine is tracking.
func (e *Engine) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	snapshot := make(map[string]string, len(e.containers))
	for k, v := range e.containers {
		snapshot[k] = v
	}
	e.mu.Unlock()

	var firstErr error
	for name, id := range snapshot {
		e.logger.Info("shutdown: removing runner",
			slog.String("name", name),
			slog.String("containerID", id),
		)
		if err := e.client.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
			e.logger.Error("shutdown: failed to remove runner",
				slog.String("name", name),
				slog.String("containerID", id),
				slog.String("error", err.Error()),
			)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	e.mu.Lock()
	clear(e.containers)
	e.mu.Unlock()

	return firstErr
}
