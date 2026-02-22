package scaler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/actions/scaleset"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

// ---------------------------------------------------------------------------
// Mock engine
// ---------------------------------------------------------------------------

type mockEngine struct {
	mu        sync.Mutex
	started   []string          // runner names passed to StartRunner
	ids       map[string]string // name -> id (for tracking)
	destroyed []string          // ids passed to DestroyRunner
	shutdown  bool

	startErr   error // if set, StartRunner returns this error
	destroyErr error // if set, DestroyRunner returns this error
	nextID     int   // auto-incrementing ID
}

func newMockEngine() *mockEngine {
	return &mockEngine{
		ids: make(map[string]string),
	}
}

func (m *mockEngine) StartRunner(_ context.Context, name string, _ string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.startErr != nil {
		return "", m.startErr
	}

	m.nextID++
	id := fmt.Sprintf("mock-id-%d", m.nextID)
	m.started = append(m.started, name)
	m.ids[name] = id
	return id, nil
}

func (m *mockEngine) DestroyRunner(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.destroyErr != nil {
		return m.destroyErr
	}

	m.destroyed = append(m.destroyed, id)
	return nil
}

func (m *mockEngine) Shutdown(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shutdown = true
	return nil
}

func (m *mockEngine) startedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.started)
}

func (m *mockEngine) destroyedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.destroyed)
}

func (m *mockEngine) getDestroyed() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Return a copy to avoid races
	result := make([]string, len(m.destroyed))
	copy(result, m.destroyed)
	return result
}

func (m *mockEngine) getStarted() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Return a copy to avoid races
	result := make([]string, len(m.started))
	copy(result, m.started)
	return result
}

// ---------------------------------------------------------------------------
// Mock JIT config generator
// ---------------------------------------------------------------------------

type mockJitGenerator struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (m *mockJitGenerator) GenerateJitRunnerConfig(
	_ context.Context,
	setting *scaleset.RunnerScaleSetJitRunnerSetting,
	_ int,
) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.err != nil {
		return nil, m.err
	}

	m.calls++
	return &scaleset.RunnerScaleSetJitRunnerConfig{
		EncodedJITConfig: fmt.Sprintf("jit-config-for-%s", setting.Name),
	}, nil
}

// ---------------------------------------------------------------------------
// Test suite
// ---------------------------------------------------------------------------

type ScalerSuite struct {
	suite.Suite
	ctx    context.Context
	engine *mockEngine
	jitGen *mockJitGenerator
	logger *slog.Logger
}

func (s *ScalerSuite) SetupTest() {
	s.ctx = context.Background()
	s.engine = newMockEngine()
	s.jitGen = &mockJitGenerator{}
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (s *ScalerSuite) newScaler(min, max int) *Scaler {
	return New(Config{
		ScaleSetID:     1,
		MinRunners:     min,
		MaxRunners:     max,
		ScalesetClient: s.jitGen,
		Engine:         s.engine,
		Logger:         s.logger,
	})
}

func TestScalerSuite(t *testing.T) {
	suite.Run(t, new(ScalerSuite))
}

// ---------------------------------------------------------------------------
// Scale-up tests
// ---------------------------------------------------------------------------

func (s *ScalerSuite) TestScaleUp_SingleRunner() {
	sc := s.newScaler(0, 10)

	count, err := sc.HandleDesiredRunnerCount(s.ctx, 1)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 1, count)
	assert.Equal(s.T(), 1, s.engine.startedCount())
	assert.Equal(s.T(), 1, len(sc.idle))
	assert.Equal(s.T(), 0, len(sc.busy))
}

func (s *ScalerSuite) TestScaleUp_MultipleRunners() {
	sc := s.newScaler(0, 10)

	count, err := sc.HandleDesiredRunnerCount(s.ctx, 5)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 5, count)
	assert.Equal(s.T(), 5, s.engine.startedCount())
	assert.Equal(s.T(), 5, len(sc.idle))
}

func (s *ScalerSuite) TestScaleUp_RespectsMaxRunners() {
	sc := s.newScaler(0, 5)

	// Request 20 runners, but max is 5
	count, err := sc.HandleDesiredRunnerCount(s.ctx, 20)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 5, count)
	assert.Equal(s.T(), 5, s.engine.startedCount())
}

func (s *ScalerSuite) TestScaleUp_RespectsMinRunners() {
	sc := s.newScaler(2, 10)

	// Request 0 desired, but min is 2 -> target = min(10, 2+0) = 2
	count, err := sc.HandleDesiredRunnerCount(s.ctx, 0)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 2, count)
	assert.Equal(s.T(), 2, s.engine.startedCount())
}

func (s *ScalerSuite) TestScaleUp_MinPlusDesired() {
	sc := s.newScaler(2, 10)

	// Request 3 desired with min=2 -> target = min(10, 2+3) = 5
	count, err := sc.HandleDesiredRunnerCount(s.ctx, 3)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 5, count)
	assert.Equal(s.T(), 5, s.engine.startedCount())
}

func (s *ScalerSuite) TestScaleUp_MaxCapsMinPlusDesired() {
	sc := s.newScaler(3, 5)

	// Request 10 desired with min=3, max=5 -> target = min(5, 3+10) = 5
	count, err := sc.HandleDesiredRunnerCount(s.ctx, 10)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 5, count)
	assert.Equal(s.T(), 5, s.engine.startedCount())
}

// ---------------------------------------------------------------------------
// Scale-down tests
// ---------------------------------------------------------------------------

func (s *ScalerSuite) TestScaleDown_Implicit() {
	sc := s.newScaler(0, 10)

	// First scale up to 5
	_, err := sc.HandleDesiredRunnerCount(s.ctx, 5)
	require.NoError(s.T(), err)

	// Now desired drops to 1 -> target = min(10, 0+1) = 1
	// But we already have 5, so scale down is implicit (no runners destroyed)
	count, err := sc.HandleDesiredRunnerCount(s.ctx, 1)
	require.NoError(s.T(), err)

	// Returns current count (5), no runners destroyed -- they drain naturally
	assert.Equal(s.T(), 5, count)
	assert.Equal(s.T(), 0, s.engine.destroyedCount())
}

func (s *ScalerSuite) TestNoScaling_WhenAtTarget() {
	sc := s.newScaler(0, 10)

	// Scale up to 3
	_, err := sc.HandleDesiredRunnerCount(s.ctx, 3)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 3, s.engine.startedCount())

	// Request exactly 3 again -> no new runners
	count, err := sc.HandleDesiredRunnerCount(s.ctx, 3)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 3, count)
	assert.Equal(s.T(), 3, s.engine.startedCount()) // still 3, no new starts
}

// ---------------------------------------------------------------------------
// Job lifecycle tests
// ---------------------------------------------------------------------------

func (s *ScalerSuite) TestHandleJobStarted_MovesToBusy() {
	sc := s.newScaler(0, 10)

	// Scale up to get a runner
	_, err := sc.HandleDesiredRunnerCount(s.ctx, 1)
	require.NoError(s.T(), err)

	// Get the runner name from the idle map
	var runnerName string
	for name := range sc.idle {
		runnerName = name
	}
	require.NotEmpty(s.T(), runnerName)

	err = sc.HandleJobStarted(s.ctx, &scaleset.JobStarted{
		RunnerName: runnerName,
	})
	require.NoError(s.T(), err)

	assert.Equal(s.T(), 0, len(sc.idle))
	assert.Equal(s.T(), 1, len(sc.busy))
	assert.Contains(s.T(), sc.busy, runnerName)
}

func (s *ScalerSuite) TestHandleJobStarted_UnknownRunner() {
	sc := s.newScaler(0, 10)

	// Job started for a runner we don't know about -- should not error
	err := sc.HandleJobStarted(s.ctx, &scaleset.JobStarted{
		RunnerName: "unknown-runner",
	})
	require.NoError(s.T(), err)
}

func (s *ScalerSuite) TestHandleJobCompleted_DestroysRunner() {
	sc := s.newScaler(0, 10)

	// Scale up, start job, complete job
	_, err := sc.HandleDesiredRunnerCount(s.ctx, 1)
	require.NoError(s.T(), err)

	var runnerName string
	for name := range sc.idle {
		runnerName = name
	}

	err = sc.HandleJobStarted(s.ctx, &scaleset.JobStarted{
		RunnerName: runnerName,
	})
	require.NoError(s.T(), err)

	err = sc.HandleJobCompleted(s.ctx, &scaleset.JobCompleted{
		RunnerName: runnerName,
		Result:     "success",
	})
	require.NoError(s.T(), err)

	assert.Equal(s.T(), 0, len(sc.idle))
	assert.Equal(s.T(), 0, len(sc.busy))
	assert.Equal(s.T(), 1, s.engine.destroyedCount())
}

func (s *ScalerSuite) TestHandleJobCompleted_UnknownRunner() {
	sc := s.newScaler(0, 10)

	// Complete a job for a runner we don't know -- should not error, no engine call
	err := sc.HandleJobCompleted(s.ctx, &scaleset.JobCompleted{
		RunnerName: "unknown-runner",
		Result:     "success",
	})
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 0, s.engine.destroyedCount())
}

// ---------------------------------------------------------------------------
// Full lifecycle: multiple jobs
// ---------------------------------------------------------------------------

func (s *ScalerSuite) TestFullLifecycle_MultipleJobs() {
	sc := s.newScaler(0, 10)

	// Scale up 3 runners
	count, err := sc.HandleDesiredRunnerCount(s.ctx, 3)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 3, count)

	// Collect runner names
	runners := make([]string, 0, 3)
	for name := range sc.idle {
		runners = append(runners, name)
	}
	require.Len(s.T(), runners, 3)

	// Start all 3 jobs
	for _, name := range runners {
		err := sc.HandleJobStarted(s.ctx, &scaleset.JobStarted{
			RunnerName: name,
		})
		require.NoError(s.T(), err)
	}
	assert.Equal(s.T(), 0, len(sc.idle))
	assert.Equal(s.T(), 3, len(sc.busy))

	// Complete all 3 jobs
	for _, name := range runners {
		err := sc.HandleJobCompleted(s.ctx, &scaleset.JobCompleted{
			RunnerName: name,
			Result:     "success",
		})
		require.NoError(s.T(), err)
	}

	// All runners destroyed, maps empty
	assert.Equal(s.T(), 0, len(sc.idle))
	assert.Equal(s.T(), 0, len(sc.busy))
	assert.Equal(s.T(), 3, s.engine.destroyedCount())
	assert.Equal(s.T(), 0, sc.runnerCount())
}

func (s *ScalerSuite) TestFullLifecycle_ScaleUpAgainAfterCompletion() {
	sc := s.newScaler(0, 10)

	// First wave: 2 runners, run jobs, complete
	_, err := sc.HandleDesiredRunnerCount(s.ctx, 2)
	require.NoError(s.T(), err)

	runners := make([]string, 0, 2)
	for name := range sc.idle {
		runners = append(runners, name)
	}

	for _, name := range runners {
		_ = sc.HandleJobStarted(s.ctx, &scaleset.JobStarted{RunnerName: name})
		_ = sc.HandleJobCompleted(s.ctx, &scaleset.JobCompleted{RunnerName: name, Result: "success"})
	}
	assert.Equal(s.T(), 0, sc.runnerCount())

	// Second wave: 3 more runners
	count, err := sc.HandleDesiredRunnerCount(s.ctx, 3)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 3, count)
	assert.Equal(s.T(), 5, s.engine.startedCount()) // 2 + 3
	assert.Equal(s.T(), 3, len(sc.idle))
}

// ---------------------------------------------------------------------------
// Concurrent access
// ---------------------------------------------------------------------------

func (s *ScalerSuite) TestConcurrentScaling() {
	sc := s.newScaler(0, 100)

	// Run many concurrent scaling operations to verify no races.
	// This test is most valuable when run with -race.
	var wg sync.WaitGroup
	var ops atomic.Int64

	// 10 concurrent scale-up goroutines
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := sc.HandleDesiredRunnerCount(s.ctx, 1)
			if err == nil {
				ops.Add(1)
			}
		}()
	}
	wg.Wait()

	// All operations should succeed (no panics, no data races)
	assert.Greater(s.T(), ops.Load(), int64(0))
	assert.Greater(s.T(), sc.runnerCount(), 0)

	// Now do concurrent job starts and completions
	sc2 := s.newScaler(0, 50)
	_, err := sc2.HandleDesiredRunnerCount(s.ctx, 20)
	require.NoError(s.T(), err)

	runners := make([]string, 0)
	for name := range sc2.idle {
		runners = append(runners, name)
	}

	// Start all jobs concurrently
	for _, name := range runners {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			_ = sc2.HandleJobStarted(s.ctx, &scaleset.JobStarted{RunnerName: n})
		}(name)
	}
	wg.Wait()

	assert.Equal(s.T(), 0, len(sc2.idle))
	assert.Equal(s.T(), 20, len(sc2.busy))

	// Complete all jobs concurrently
	for _, name := range runners {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			_ = sc2.HandleJobCompleted(s.ctx, &scaleset.JobCompleted{RunnerName: n, Result: "success"})
		}(name)
	}
	wg.Wait()

	assert.Equal(s.T(), 0, sc2.runnerCount())
}

// ---------------------------------------------------------------------------
// Shutdown
// ---------------------------------------------------------------------------

func (s *ScalerSuite) TestShutdown_CleansUpAll() {
	sc := s.newScaler(0, 10)

	_, err := sc.HandleDesiredRunnerCount(s.ctx, 3)
	require.NoError(s.T(), err)

	// Move one to busy
	var firstRunner string
	for name := range sc.idle {
		firstRunner = name
		break
	}
	_ = sc.HandleJobStarted(s.ctx, &scaleset.JobStarted{RunnerName: firstRunner})

	assert.Equal(s.T(), 2, len(sc.idle))
	assert.Equal(s.T(), 1, len(sc.busy))

	sc.Shutdown(s.ctx)

	assert.True(s.T(), s.engine.shutdown)
	assert.Equal(s.T(), 0, len(sc.idle))
	assert.Equal(s.T(), 0, len(sc.busy))
}

// ---------------------------------------------------------------------------
// Error handling
// ---------------------------------------------------------------------------

func (s *ScalerSuite) TestScaleUp_EngineFailure() {
	s.engine.startErr = fmt.Errorf("docker daemon unavailable")
	sc := s.newScaler(0, 10)

	count, err := sc.HandleDesiredRunnerCount(s.ctx, 3)

	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "docker daemon unavailable")
	// Should return partial count (0 since engine fails on first attempt)
	assert.Equal(s.T(), 0, count)
}

func (s *ScalerSuite) TestScaleUp_EngineFailureMidScale() {
	sc := s.newScaler(0, 10)

	// Let 2 runners succeed, then fail on the 3rd
	callCount := 0
	origStart := s.engine.StartRunner
	_ = origStart // avoid unused
	s.engine.startErr = nil

	// We need a more nuanced mock. Let's just do two separate
	// HandleDesiredRunnerCount calls to simulate partial success.
	_, err := sc.HandleDesiredRunnerCount(s.ctx, 2)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 2, sc.runnerCount())
	_ = callCount

	// Now make engine fail
	s.engine.startErr = fmt.Errorf("out of capacity")

	// Try to scale up by 3 more (current=2, target=min(10,0+5)=5, delta=3)
	count, err := sc.HandleDesiredRunnerCount(s.ctx, 5)
	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "out of capacity")
	// Returns whatever count we managed (still 2)
	assert.Equal(s.T(), 2, count)
}

func (s *ScalerSuite) TestScaleUp_JitConfigFailure() {
	s.jitGen.err = fmt.Errorf("github API rate limited")
	sc := s.newScaler(0, 10)

	count, err := sc.HandleDesiredRunnerCount(s.ctx, 1)

	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "github API rate limited")
	assert.Equal(s.T(), 0, count)
	// Engine should never have been called since JIT config failed
	assert.Equal(s.T(), 0, s.engine.startedCount())
}

func (s *ScalerSuite) TestHandleJobCompleted_DestroyError() {
	s.engine.destroyErr = fmt.Errorf("container already gone")
	sc := s.newScaler(0, 10)

	_, err := sc.HandleDesiredRunnerCount(s.ctx, 1)
	require.NoError(s.T(), err)

	var runnerName string
	for name := range sc.idle {
		runnerName = name
	}

	_ = sc.HandleJobStarted(s.ctx, &scaleset.JobStarted{RunnerName: runnerName})

	err = sc.HandleJobCompleted(s.ctx, &scaleset.JobCompleted{
		RunnerName: runnerName,
		Result:     "success",
	})
	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "container already gone")
}

// ---------------------------------------------------------------------------
// One-runner-per-job correctness tests
// ---------------------------------------------------------------------------

func (s *ScalerSuite) TestOneRunnerPerJob_Sequential() {
	// Test the core invariant: exactly one runner is created and destroyed
	// per job, even when jobs run sequentially.
	const N = 50
	sc := s.newScaler(0, 100)

	// Scale up N runners
	count, err := sc.HandleDesiredRunnerCount(s.ctx, N)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), N, count)
	assert.Equal(s.T(), N, s.engine.startedCount())
	assert.Equal(s.T(), N, len(sc.idle))
	assert.Equal(s.T(), 0, len(sc.busy))

	// Collect runner names
	runners := make([]string, 0, N)
	for name := range sc.idle {
		runners = append(runners, name)
	}
	require.Len(s.T(), runners, N)

	// For each runner: start job -> complete job
	for _, name := range runners {
		err := sc.HandleJobStarted(s.ctx, &scaleset.JobStarted{
			RunnerName: name,
		})
		require.NoError(s.T(), err)
		assert.Contains(s.T(), sc.busy, name)

		err = sc.HandleJobCompleted(s.ctx, &scaleset.JobCompleted{
			RunnerName: name,
			Result:     "success",
		})
		require.NoError(s.T(), err)
	}

	// Assert: all runners created and destroyed, maps empty
	assert.Equal(s.T(), N, s.engine.startedCount())
	assert.Equal(s.T(), N, s.engine.destroyedCount())
	assert.Equal(s.T(), 0, len(sc.idle))
	assert.Equal(s.T(), 0, len(sc.busy))
	assert.Equal(s.T(), 0, sc.runnerCount())

	// Assert: no duplicate IDs in destroyed slice
	destroyed := s.engine.getDestroyed()
	uniqueIDs := make(map[string]bool)
	for _, id := range destroyed {
		assert.False(s.T(), uniqueIDs[id], "duplicate destroy for ID %s", id)
		uniqueIDs[id] = true
	}
	assert.Len(s.T(), uniqueIDs, N)
}

func (s *ScalerSuite) TestOneRunnerPerJob_ConcurrentJobs() {
	// Stress test: verify the 1:1 runner-to-job mapping under concurrent load.
	// This is the most critical test for the core invariant.
	// Run with -race to detect data races.
	const N = 100
	sc := s.newScaler(0, 150)

	// Scale up N runners
	count, err := sc.HandleDesiredRunnerCount(s.ctx, N)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), N, count)

	// Collect runner names
	runners := make([]string, 0, N)
	for name := range sc.idle {
		runners = append(runners, name)
	}
	require.Len(s.T(), runners, N)

	var wg sync.WaitGroup

	// Phase 1: Concurrently start all jobs
	for _, name := range runners {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			err := sc.HandleJobStarted(s.ctx, &scaleset.JobStarted{
				RunnerName: n,
			})
			assert.NoError(s.T(), err)
		}(name)
	}
	wg.Wait()

	// All runners moved from idle -> busy
	assert.Equal(s.T(), 0, len(sc.idle))
	assert.Equal(s.T(), N, len(sc.busy))

	// Phase 2: Concurrently complete all jobs
	for _, name := range runners {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			err := sc.HandleJobCompleted(s.ctx, &scaleset.JobCompleted{
				RunnerName: n,
				Result:     "success",
			})
			assert.NoError(s.T(), err)
		}(name)
	}
	wg.Wait()

	// Assert: exactly N starts and N destroys
	assert.Equal(s.T(), N, s.engine.startedCount())
	assert.Equal(s.T(), N, s.engine.destroyedCount())
	assert.Equal(s.T(), 0, sc.runnerCount())

	// Assert: no duplicate IDs in destroyed slice
	destroyed := s.engine.getDestroyed()
	uniqueIDs := make(map[string]bool)
	for _, id := range destroyed {
		assert.False(s.T(), uniqueIDs[id], "duplicate destroy for ID %s", id)
		uniqueIDs[id] = true
	}
	assert.Len(s.T(), uniqueIDs, N)

	// Assert: every started runner was destroyed
	started := s.engine.getStarted()
	for _, name := range started {
		// Get the engine ID for this runner name
		s.engine.mu.Lock()
		id, exists := s.engine.ids[name]
		s.engine.mu.Unlock()
		require.True(s.T(), exists, "runner %s has no engine ID", name)
		assert.True(s.T(), uniqueIDs[id], "runner %s (ID %s) was started but not destroyed", name, id)
	}
}

func (s *ScalerSuite) TestOneRunnerPerJob_InterleavedLifecycles() {
	// Test realistic interleaving: some jobs complete while others are starting,
	// with multiple waves of scaling.
	sc := s.newScaler(0, 100)

	// Wave 1: Scale up 20 runners
	_, err := sc.HandleDesiredRunnerCount(s.ctx, 20)
	require.NoError(s.T(), err)

	// Collect first batch
	wave1 := make([]string, 0)
	for name := range sc.idle {
		wave1 = append(wave1, name)
	}
	require.Len(s.T(), wave1, 20)

	// Start half of wave1 jobs
	for i := 0; i < 10; i++ {
		_ = sc.HandleJobStarted(s.ctx, &scaleset.JobStarted{RunnerName: wave1[i]})
	}
	assert.Equal(s.T(), 10, len(sc.idle))
	assert.Equal(s.T(), 10, len(sc.busy))

	// Wave 2: Scale up 30 more runners (while wave1 jobs are running)
	_, err = sc.HandleDesiredRunnerCount(s.ctx, 50)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 50, sc.runnerCount())

	// Collect wave2 (new runners only)
	wave2 := make([]string, 0)
	for name := range sc.idle {
		found := false
		for _, w1name := range wave1 {
			if name == w1name {
				found = true
				break
			}
		}
		if !found {
			wave2 = append(wave2, name)
		}
	}

	// Complete the first 10 jobs from wave1
	for i := 0; i < 10; i++ {
		_ = sc.HandleJobCompleted(s.ctx, &scaleset.JobCompleted{
			RunnerName: wave1[i],
			Result:     "success",
		})
	}
	assert.Equal(s.T(), 10, s.engine.destroyedCount())

	// Start remaining wave1 jobs
	for i := 10; i < 20; i++ {
		_ = sc.HandleJobStarted(s.ctx, &scaleset.JobStarted{RunnerName: wave1[i]})
	}

	// Start all wave2 jobs
	for _, name := range wave2 {
		_ = sc.HandleJobStarted(s.ctx, &scaleset.JobStarted{RunnerName: name})
	}

	// At this point: 10 wave1 completed, 10 wave1 running, 30 wave2 running
	// = 40 busy, 0 idle
	assert.Equal(s.T(), 0, len(sc.idle))
	assert.Equal(s.T(), 40, len(sc.busy))

	// Complete all remaining jobs
	for i := 10; i < 20; i++ {
		_ = sc.HandleJobCompleted(s.ctx, &scaleset.JobCompleted{
			RunnerName: wave1[i],
			Result:     "success",
		})
	}
	for _, name := range wave2 {
		_ = sc.HandleJobCompleted(s.ctx, &scaleset.JobCompleted{
			RunnerName: name,
			Result:     "success",
		})
	}

	// Final state: 50 runners created, 50 destroyed
	assert.Equal(s.T(), 50, s.engine.startedCount())
	assert.Equal(s.T(), 50, s.engine.destroyedCount())
	assert.Equal(s.T(), 0, sc.runnerCount())

	// No duplicate destroys
	destroyed := s.engine.getDestroyed()
	uniqueIDs := make(map[string]bool)
	for _, id := range destroyed {
		assert.False(s.T(), uniqueIDs[id], "duplicate destroy for ID %s", id)
		uniqueIDs[id] = true
	}
	assert.Len(s.T(), uniqueIDs, 50)
}

func (s *ScalerSuite) TestOneRunnerPerJob_DuplicateEvents() {
	// Test idempotency: duplicate HandleJobStarted and HandleJobCompleted
	// should not create/destroy runners multiple times.
	const N = 20
	sc := s.newScaler(0, 30)

	// Scale up N runners
	_, err := sc.HandleDesiredRunnerCount(s.ctx, N)
	require.NoError(s.T(), err)

	runners := make([]string, 0, N)
	for name := range sc.idle {
		runners = append(runners, name)
	}
	require.Len(s.T(), runners, N)

	// Send duplicate JobStarted events for each runner
	for _, name := range runners {
		err := sc.HandleJobStarted(s.ctx, &scaleset.JobStarted{RunnerName: name})
		require.NoError(s.T(), err)

		// Duplicate - should be a no-op (runner already in busy)
		err = sc.HandleJobStarted(s.ctx, &scaleset.JobStarted{RunnerName: name})
		require.NoError(s.T(), err)
	}

	assert.Equal(s.T(), 0, len(sc.idle))
	assert.Equal(s.T(), N, len(sc.busy))

	// Send duplicate JobCompleted events
	for _, name := range runners {
		err := sc.HandleJobCompleted(s.ctx, &scaleset.JobCompleted{
			RunnerName: name,
			Result:     "success",
		})
		require.NoError(s.T(), err)

		// Duplicate - should be a no-op (runner already removed)
		err = sc.HandleJobCompleted(s.ctx, &scaleset.JobCompleted{
			RunnerName: name,
			Result:     "success",
		})
		require.NoError(s.T(), err)
	}

	// Assert: still exactly N creates and N destroys (no duplicates)
	assert.Equal(s.T(), N, s.engine.startedCount())
	assert.Equal(s.T(), N, s.engine.destroyedCount())
	assert.Equal(s.T(), 0, sc.runnerCount())

	// No duplicate destroys
	destroyed := s.engine.getDestroyed()
	uniqueIDs := make(map[string]bool)
	for _, id := range destroyed {
		assert.False(s.T(), uniqueIDs[id], "duplicate destroy for ID %s", id)
		uniqueIDs[id] = true
	}
	assert.Len(s.T(), uniqueIDs, N)
}
