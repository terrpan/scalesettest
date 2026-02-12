// Package scaler provides an engine-agnostic implementation of
// listener.Scaler that bridges the scaleset SDK's message lifecycle
// to any compute backend via the engine.Engine interface.
package scaler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

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

	// OpenTelemetry instrumentation
	tracer trace.Tracer
	meter  metric.Meter

	// Metrics
	runnersStarted        metric.Int64Counter
	runnersDestroyed      metric.Int64Counter
	jobsCompleted         metric.Int64Counter
	scaleEvents           metric.Int64Counter
	runnerStartupDuration metric.Float64Histogram
}

// Compile-time check.
var _ listener.Scaler = (*Scaler)(nil)

// New creates a Scaler.
func New(cfg Config) *Scaler {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(nil, nil))
	}

	s := &Scaler{
		engine:         cfg.Engine,
		scalesetClient: cfg.ScalesetClient,
		scaleSetID:     cfg.ScaleSetID,
		minRunners:     cfg.MinRunners,
		maxRunners:     cfg.MaxRunners,
		logger:         cfg.Logger,
		idle:           make(map[string]string),
		busy:           make(map[string]string),
		tracer:         otel.Tracer("scaleset/scaler"),
		meter:          otel.Meter("scaleset/scaler"),
	}

	// Initialize metrics (errors are logged but not fatal)
	var err error
	s.runnersStarted, err = s.meter.Int64Counter(
		"scaleset.runners.started",
		metric.WithDescription("Total number of runners started"),
		metric.WithUnit("1"),
	)
	if err != nil {
		cfg.Logger.Warn("failed to create runnersStarted counter", slog.String("error", err.Error()))
	}

	s.runnersDestroyed, err = s.meter.Int64Counter(
		"scaleset.runners.destroyed",
		metric.WithDescription("Total number of runners destroyed"),
		metric.WithUnit("1"),
	)
	if err != nil {
		cfg.Logger.Warn("failed to create runnersDestroyed counter", slog.String("error", err.Error()))
	}

	s.jobsCompleted, err = s.meter.Int64Counter(
		"scaleset.jobs.completed",
		metric.WithDescription("Total number of jobs completed"),
		metric.WithUnit("1"),
	)
	if err != nil {
		cfg.Logger.Warn("failed to create jobsCompleted counter", slog.String("error", err.Error()))
	}

	s.scaleEvents, err = s.meter.Int64Counter(
		"scaleset.scale.events",
		metric.WithDescription("Total number of scale events"),
		metric.WithUnit("1"),
	)
	if err != nil {
		cfg.Logger.Warn("failed to create scaleEvents counter", slog.String("error", err.Error()))
	}

	s.runnerStartupDuration, err = s.meter.Float64Histogram(
		"scaleset.runner.startup.duration",
		metric.WithDescription("Time to start a runner (seconds)"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 30, 60, 120, 300),
	)
	if err != nil {
		cfg.Logger.Warn("failed to create runnerStartupDuration histogram", slog.String("error", err.Error()))
	}

	// Register observable gauges for idle/busy runner counts
	_, err = s.meter.Int64ObservableGauge(
		"scaleset.runners.idle",
		metric.WithDescription("Current number of idle runners"),
		metric.WithUnit("1"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			s.mu.Lock()
			count := len(s.idle)
			s.mu.Unlock()
			o.Observe(int64(count))
			return nil
		}),
	)
	if err != nil {
		cfg.Logger.Warn("failed to create idle gauge", slog.String("error", err.Error()))
	}

	_, err = s.meter.Int64ObservableGauge(
		"scaleset.runners.busy",
		metric.WithDescription("Current number of busy runners"),
		metric.WithUnit("1"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			s.mu.Lock()
			count := len(s.busy)
			s.mu.Unlock()
			o.Observe(int64(count))
			return nil
		}),
	)
	if err != nil {
		cfg.Logger.Warn("failed to create busy gauge", slog.String("error", err.Error()))
	}

	return s
}

// ---------------------------------------------------------------------------
// listener.Scaler implementation
// ---------------------------------------------------------------------------

// HandleDesiredRunnerCount is called by the listener each time the
// scaleset API reports how many runners are needed.
func (s *Scaler) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	ctx, span := s.tracer.Start(ctx, "scaler.HandleDesiredRunnerCount")
	defer span.End()

	s.mu.Lock()
	currentCount := len(s.idle) + len(s.busy)
	s.mu.Unlock()

	targetCount := min(s.maxRunners, s.minRunners+count)

	span.SetAttributes(
		attribute.Int("scaleset.desired_count", count),
		attribute.Int("scaleset.current_count", currentCount),
		attribute.Int("scaleset.target_count", targetCount),
	)

	switch {
	case targetCount == currentCount:
		span.SetAttributes(attribute.String("scaleset.scale_action", "none"))
		if s.scaleEvents != nil {
			s.scaleEvents.Add(ctx, 1, metric.WithAttributes(attribute.String("action", "none")))
		}
		s.logger.Debug("no scaling action needed",
			slog.Int("current", currentCount),
			slog.Int("target", targetCount),
		)
		return currentCount, nil

	case targetCount > currentCount:
		delta := targetCount - currentCount
		span.SetAttributes(
			attribute.String("scaleset.scale_action", "up"),
			attribute.Int("scaleset.scale_delta", delta),
		)
		if s.scaleEvents != nil {
			s.scaleEvents.Add(ctx, 1, metric.WithAttributes(attribute.String("action", "up")))
		}
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
		span.SetAttributes(attribute.String("scaleset.scale_action", "down"))
		if s.scaleEvents != nil {
			s.scaleEvents.Add(ctx, 1, metric.WithAttributes(attribute.String("action", "down")))
		}
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
	ctx, span := s.tracer.Start(ctx, "scaler.HandleJobStarted")
	defer span.End()

	span.SetAttributes(
		attribute.String("runner.name", jobInfo.RunnerName),
		attribute.Int64("job.runner_request_id", jobInfo.RunnerRequestID),
		attribute.String("job.id", jobInfo.JobID),
		attribute.String("job.display_name", jobInfo.JobDisplayName),
	)

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
	ctx, span := s.tracer.Start(ctx, "scaler.HandleJobCompleted")
	defer span.End()

	span.SetAttributes(
		attribute.String("runner.name", jobInfo.RunnerName),
		attribute.Int64("job.runner_request_id", jobInfo.RunnerRequestID),
		attribute.String("job.id", jobInfo.JobID),
		attribute.String("job.result", jobInfo.Result),
	)

	if s.jobsCompleted != nil {
		s.jobsCompleted.Add(ctx, 1, metric.WithAttributes(attribute.String("result", jobInfo.Result)))
	}

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

	if s.runnersDestroyed != nil {
		s.runnersDestroyed.Add(ctx, 1)
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
	ctx, span := s.tracer.Start(ctx, "scaler.startRunner")
	defer span.End()

	startTime := time.Now()

	name := fmt.Sprintf("runner-%s", uuid.NewString()[:8])
	span.SetAttributes(attribute.String("runner.name", name))

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

	// Record startup duration
	duration := time.Since(startTime).Seconds()
	if s.runnerStartupDuration != nil {
		s.runnerStartupDuration.Record(ctx, duration)
	}

	if s.runnersStarted != nil {
		s.runnersStarted.Add(ctx, 1)
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
