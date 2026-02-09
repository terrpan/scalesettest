// Package scaler provides an engine-agnostic implementation of
// listener.Scaler that bridges the scaleset SDK's message lifecycle
// to any compute backend via the engine.Engine interface.
package scaler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"

	"github.com/terrpan/scaleset/internal/engine"
)

// Config holds the parameters the Scaler needs that are not
// engine-specific.
type Config struct {
	ScaleSetID     int
	MinRunners     int
	MaxRunners     int
	ScalesetClient *scaleset.Client
	Engine         engine.Engine
	Logger         *slog.Logger
}

// Scaler implements listener.Scaler.  It tracks runner state (idle vs
// busy) and delegates provisioning / cleanup to the configured Engine.
type Scaler struct {
	engine         engine.Engine
	scalesetClient *scaleset.Client
	scaleSetID     int
	minRunners     int
	maxRunners     int
	logger         *slog.Logger

	mu   sync.Mutex
	idle map[string]string // runner name -> engine id
	busy map[string]string // runner name -> engine id
}

// Compile-time check.
var _ listener.Scaler = (*Scaler)(nil)

// New creates a Scaler.
func New(cfg Config) *Scaler {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Scaler{
		engine:         cfg.Engine,
		scalesetClient: cfg.ScalesetClient,
		scaleSetID:     cfg.ScaleSetID,
		minRunners:     cfg.MinRunners,
		maxRunners:     cfg.MaxRunners,
		logger:         cfg.Logger,
		idle:           make(map[string]string),
		busy:           make(map[string]string),
	}
}

// ---------------------------------------------------------------------------
// listener.Scaler implementation
// ---------------------------------------------------------------------------

// HandleDesiredRunnerCount is called by the listener each time the
// scaleset API reports how many runners are needed.
func (s *Scaler) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	s.mu.Lock()
	currentCount := len(s.idle) + len(s.busy)
	s.mu.Unlock()

	targetCount := min(s.maxRunners, s.minRunners+count)

	switch {
	case targetCount == currentCount:
		s.logger.Debug("no scaling action needed",
			slog.Int("current", currentCount),
			slog.Int("target", targetCount),
		)
		return currentCount, nil

	case targetCount > currentCount:
		delta := targetCount - currentCount
		s.logger.Info("scaling up",
			slog.Int("current", currentCount),
			slog.Int("target", targetCount),
			slog.Int("delta", delta),
		)

		for range delta {
			if _, err := s.startRunner(ctx); err != nil {
				return s.runnerCount(), fmt.Errorf("start runner: %w", err)
			}
		}
		return s.runnerCount(), nil

	default:
		// Scale-down is handled implicitly: runners are ephemeral and
		// are removed on JobCompleted.  If the desired count drops,
		// we simply stop creating new ones -- the existing ones will
		// drain naturally.
		s.logger.Debug("scale down signalled, waiting for jobs to complete",
			slog.Int("current", currentCount),
			slog.Int("target", targetCount),
		)
		return currentCount, nil
	}
}

// HandleJobStarted is called when GitHub assigns a job to one of our
// runners.
func (s *Scaler) HandleJobStarted(ctx context.Context, jobInfo *scaleset.JobStarted) error {
	s.logger.Info("job started",
		slog.String("runner", jobInfo.RunnerName),
		slog.Int64("runnerRequestID", jobInfo.RunnerRequestID),
		slog.String("jobID", jobInfo.JobID),
		slog.String("jobDisplayName", jobInfo.JobDisplayName),
		slog.String("repo", jobInfo.RepositoryName),
	)

	s.mu.Lock()
	defer s.mu.Unlock()

	id, ok := s.idle[jobInfo.RunnerName]
	if !ok {
		// This can happen if the runner was already marked busy via a
		// duplicate message.  Log a warning but do not fail.
		s.logger.Warn("job started for unknown/already-busy runner",
			slog.String("runner", jobInfo.RunnerName),
		)
		return nil
	}
	delete(s.idle, jobInfo.RunnerName)
	s.busy[jobInfo.RunnerName] = id
	return nil
}

// HandleJobCompleted is called when a job finishes.  The runner is
// ephemeral so we tear it down immediately.
func (s *Scaler) HandleJobCompleted(ctx context.Context, jobInfo *scaleset.JobCompleted) error {
	s.logger.Info("job completed",
		slog.String("runner", jobInfo.RunnerName),
		slog.Int64("runnerRequestID", jobInfo.RunnerRequestID),
		slog.String("jobID", jobInfo.JobID),
		slog.String("result", jobInfo.Result),
		slog.String("repo", jobInfo.RepositoryName),
	)

	id := s.removeRunner(jobInfo.RunnerName)
	if id == "" {
		s.logger.Warn("job completed for unknown runner",
			slog.String("runner", jobInfo.RunnerName),
		)
		return nil
	}

	if err := s.engine.DestroyRunner(ctx, id); err != nil {
		return fmt.Errorf("destroy runner %s (%s): %w", jobInfo.RunnerName, id, err)
	}
	return nil
}

// Shutdown tears down all runners via the engine.
func (s *Scaler) Shutdown(ctx context.Context) {
	s.logger.Info("shutting down all runners")
	if err := s.engine.Shutdown(ctx); err != nil {
		s.logger.Error("engine shutdown error", slog.String("error", err.Error()))
	}

	s.mu.Lock()
	clear(s.idle)
	clear(s.busy)
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// internal helpers
// ---------------------------------------------------------------------------

func (s *Scaler) startRunner(ctx context.Context) (string, error) {
	name := fmt.Sprintf("runner-%s", uuid.NewString()[:8])

	jit, err := s.scalesetClient.GenerateJitRunnerConfig(
		ctx,
		&scaleset.RunnerScaleSetJitRunnerSetting{
			Name: name,
		},
		s.scaleSetID,
	)
	if err != nil {
		return "", fmt.Errorf("generate JIT config for %s: %w", name, err)
	}

	id, err := s.engine.StartRunner(ctx, name, jit.EncodedJITConfig)
	if err != nil {
		return "", fmt.Errorf("engine start %s: %w", name, err)
	}

	s.mu.Lock()
	s.idle[name] = id
	s.mu.Unlock()

	return name, nil
}

func (s *Scaler) removeRunner(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if id, ok := s.busy[name]; ok {
		delete(s.busy, name)
		return id
	}
	if id, ok := s.idle[name]; ok {
		delete(s.idle, name)
		return id
	}
	return ""
}

func (s *Scaler) runnerCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.idle) + len(s.busy)
}
