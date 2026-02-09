// Package engine defines the abstraction for compute backends that run
// GitHub Actions runners. Each backend (Docker, EC2, Azure VMs, GCP, etc.)
// implements the Engine interface so the rest of the system remains
// compute-agnostic.
package engine

import "context"

// Engine is the contract every compute backend must satisfy.
//
// All runners are strictly ephemeral: each runner executes exactly one
// job and is then permanently destroyed (not stopped, not paused).
// The full lifecycle is:
//
//	StartRunner → idle → (job assigned) → busy → (job done) → DestroyRunner
//
// Implementations are responsible for launching a runner process with
// the provided JIT configuration and fully destroying it when the job
// completes.  The returned id is opaque to callers -- it may be a
// Docker container ID, an EC2 instance ID, a Kubernetes pod name, etc.
type Engine interface {
	// StartRunner provisions and starts a new ephemeral GitHub Actions
	// runner.
	//
	// name is a human-readable identifier used both as the runner
	// registration name and (where applicable) as the resource name
	// in the compute backend.
	//
	// jitConfig is the base64-encoded JIT configuration obtained from
	// the scaleset API via GenerateJitRunnerConfig.
	//
	// The returned id uniquely identifies the runner within the
	// backend and is passed back to DestroyRunner when the job completes.
	StartRunner(ctx context.Context, name string, jitConfig string) (id string, err error)

	// DestroyRunner permanently destroys the runner identified by id.
	// For Docker this means force-removing the container; for VMs this
	// means terminating the instance -- never merely stopping it.
	// It must be idempotent -- calling DestroyRunner on an
	// already-destroyed runner should not return an error.
	DestroyRunner(ctx context.Context, id string) error

	// Shutdown permanently destroys all runners currently managed by
	// this engine instance.  It is called once during process
	// termination.
	Shutdown(ctx context.Context) error
}
