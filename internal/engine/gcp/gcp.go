// Package gcp implements the engine.Engine interface using Google Cloud
// Compute Engine to run ephemeral GitHub Actions runners as VMs.
//
// Authentication uses Application Default Credentials (ADC).  No
// credential fields exist in Config -- auth is handled by the
// environment (attached service account, Workload Identity Federation,
// GOOGLE_APPLICATION_CREDENTIALS, or gcloud auth application-default login).
package gcp

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	"github.com/terrpan/scaleset/internal/engine"
)

// Config holds GCP-specific engine settings.
type Config struct {
	// Project is the GCP project ID (required).
	Project string

	// Zone is the GCP zone where runner VMs are created (required).
	Zone string

	// MachineType is the Compute Engine machine type.
	// Default: "e2-medium".
	MachineType string

	// Image is the full self-link or family URL of the runner image (required).
	// Examples:
	//   "projects/my-project/global/images/scaleset-runner-1234567890"
	//   "projects/my-project/global/images/family/scaleset-runner"
	Image string

	// DiskSizeGB is the boot disk size in GB.  Default: 50.
	DiskSizeGB int64

	// Network is the VPC network (optional).  Defaults to "default".
	Network string

	// Subnet is the subnetwork (optional).  If empty, the default subnet
	// for the zone is used.
	Subnet string

	// PublicIP controls whether runner VMs get an external IP.
	// Default: true.
	PublicIP bool

	// ServiceAccount is the GCP service account email to attach to
	// runner VMs (optional).  If empty, the project's default compute
	// service account is used.
	ServiceAccount string
}

// Engine manages GitHub Actions runners as GCP Compute Engine VMs.
type Engine struct {
	client   *compute.InstancesClient
	opClient *compute.ZoneOperationsClient
	cfg      Config
	logger   *slog.Logger

	mu        sync.Mutex
	instances map[string]string // runner name -> instance name

	// OpenTelemetry instrumentation
	tracer trace.Tracer
}

// Compile-time check that Engine satisfies the engine.Engine interface.
var _ engine.Engine = (*Engine)(nil)

// New creates a GCP engine using Application Default Credentials.
func New(ctx context.Context, cfg Config, logger *slog.Logger) (*Engine, error) {
	if cfg.MachineType == "" {
		cfg.MachineType = "e2-medium"
	}
	if cfg.DiskSizeGB == 0 {
		cfg.DiskSizeGB = 50
	}
	if cfg.Network == "" {
		cfg.Network = "default"
	}

	client, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp instances client: %w", err)
	}

	opClient, err := compute.NewZoneOperationsRESTClient(ctx)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("gcp zone operations client: %w", err)
	}

	logger.Info("gcp engine initialized",
		slog.String("project", cfg.Project),
		slog.String("zone", cfg.Zone),
		slog.String("machine_type", cfg.MachineType),
		slog.String("image", cfg.Image),
	)

	return &Engine{
		client:    client,
		opClient:  opClient,
		cfg:       cfg,
		logger:    logger,
		instances: make(map[string]string),
		tracer:    otel.Tracer("scaleset/engine/gcp"),
	}, nil
}

// StartRunner creates and starts a GCP VM that runs a GitHub Actions
// runner with the provided JIT configuration.  The JIT config is passed
// via instance metadata so the startup script can read it.
func (e *Engine) StartRunner(ctx context.Context, name string, jitConfig string) (string, error) {
	ctx, span := e.tracer.Start(ctx, "engine.gcp.StartRunner")
	defer span.End()

	span.SetAttributes(
		attribute.String("runner.name", name),
		attribute.String("gcp.project", e.cfg.Project),
		attribute.String("gcp.zone", e.cfg.Zone),
		attribute.String("gcp.machine_type", e.cfg.MachineType),
	)

	machineType := fmt.Sprintf("zones/%s/machineTypes/%s", e.cfg.Zone, e.cfg.MachineType)

	// Boot disk from the pre-built runner image.
	disk := &computepb.AttachedDisk{
		AutoDelete: proto.Bool(true),
		Boot:       proto.Bool(true),
		InitializeParams: &computepb.AttachedDiskInitializeParams{
			SourceImage: proto.String(e.cfg.Image),
			DiskSizeGb:  proto.Int64(e.cfg.DiskSizeGB),
			DiskType:    proto.String(fmt.Sprintf("zones/%s/diskTypes/pd-ssd", e.cfg.Zone)),
		},
	}

	// Network interface.
	networkURL := fmt.Sprintf("global/networks/%s", e.cfg.Network)
	nic := &computepb.NetworkInterface{
		Network: proto.String(networkURL),
	}
	if e.cfg.Subnet != "" {
		nic.Subnetwork = proto.String(e.cfg.Subnet)
	}
	if e.cfg.PublicIP {
		nic.AccessConfigs = []*computepb.AccessConfig{
			{
				Name: proto.String("External NAT"),
				Type: proto.String("ONE_TO_ONE_NAT"),
			},
		}
	}

	// Instance metadata: pass JIT config to the startup script.
	metadata := &computepb.Metadata{
		Items: []*computepb.Items{
			{
				Key:   proto.String("ACTIONS_RUNNER_INPUT_JITCONFIG"),
				Value: proto.String(jitConfig),
			},
		},
	}

	instance := &computepb.Instance{
		Name:              proto.String(name),
		MachineType:       proto.String(machineType),
		Disks:             []*computepb.AttachedDisk{disk},
		NetworkInterfaces: []*computepb.NetworkInterface{nic},
		Metadata:          metadata,
	}

	// Attach a service account if configured.
	if e.cfg.ServiceAccount != "" {
		instance.ServiceAccounts = []*computepb.ServiceAccount{
			{
				Email:  proto.String(e.cfg.ServiceAccount),
				Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
			},
		}
	}

	e.logger.Info("creating runner VM",
		slog.String("name", name),
		slog.String("machine_type", e.cfg.MachineType),
		slog.String("zone", e.cfg.Zone),
	)

	op, err := e.client.Insert(ctx, &computepb.InsertInstanceRequest{
		Project:          e.cfg.Project,
		Zone:             e.cfg.Zone,
		InstanceResource: instance,
	})
	if err != nil {
		return "", fmt.Errorf("insert instance %s: %w", name, err)
	}

	// Wait for the insert operation to complete.
	span.AddEvent("waiting for GCP operation")
	if err := op.Wait(ctx); err != nil {
		return "", fmt.Errorf("waiting for instance %s: %w", name, err)
	}

	e.mu.Lock()
	e.instances[name] = name
	e.mu.Unlock()

	span.SetAttributes(attribute.String("gcp.instance_name", name))

	e.logger.Info("runner VM started",
		slog.String("name", name),
		slog.String("zone", e.cfg.Zone),
	)

	// For GCP, the instance name is the opaque ID.
	return name, nil
}

// DestroyRunner permanently deletes the VM identified by id.
// It is idempotent -- deleting an already-deleted VM is not an error.
func (e *Engine) DestroyRunner(ctx context.Context, id string) error {
	ctx, span := e.tracer.Start(ctx, "engine.gcp.DestroyRunner")
	defer span.End()

	span.SetAttributes(
		attribute.String("gcp.instance_name", id),
		attribute.String("gcp.project", e.cfg.Project),
		attribute.String("gcp.zone", e.cfg.Zone),
	)

	e.logger.Info("destroying runner VM", slog.String("name", id))

	op, err := e.client.Delete(ctx, &computepb.DeleteInstanceRequest{
		Project:  e.cfg.Project,
		Zone:     e.cfg.Zone,
		Instance: id,
	})
	if err != nil {
		// Treat "not found" as success -- the instance is already gone.
		// The GCP client returns a googleapi.Error with Code 404.
		if isNotFound(err) {
			span.AddEvent("instance already deleted (idempotent)")
			e.logger.Info("runner VM already deleted", slog.String("name", id))
			e.removeFromTracking(id)
			return nil
		}
		return fmt.Errorf("delete instance %s: %w", id, err)
	}

	if err := op.Wait(ctx); err != nil {
		// Also handle 404 during wait -- race between delete and check.
		if isNotFound(err) {
			span.AddEvent("instance already deleted during wait (idempotent)")
			e.logger.Info("runner VM already deleted", slog.String("name", id))
			e.removeFromTracking(id)
			return nil
		}
		return fmt.Errorf("waiting for delete of %s: %w", id, err)
	}

	e.removeFromTracking(id)
	e.logger.Info("runner VM destroyed", slog.String("name", id))

	return nil
}

// Shutdown deletes all VMs currently tracked by this engine instance.
func (e *Engine) Shutdown(ctx context.Context) error {
	ctx, span := e.tracer.Start(ctx, "engine.gcp.Shutdown")
	defer span.End()

	e.mu.Lock()
	snapshot := make(map[string]string, len(e.instances))
	for k, v := range e.instances {
		snapshot[k] = v
	}
	e.mu.Unlock()

	span.SetAttributes(attribute.Int("gcp.instances_count", len(snapshot)))

	var firstErr error
	for name, id := range snapshot {
		e.logger.Info("shutdown: deleting runner VM",
			slog.String("name", name),
		)
		if err := e.DestroyRunner(ctx, id); err != nil {
			e.logger.Error("shutdown: failed to delete runner VM",
				slog.String("name", name),
				slog.String("error", err.Error()),
			)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	e.mu.Lock()
	clear(e.instances)
	e.mu.Unlock()

	// Close the API clients.
	if err := e.client.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := e.opClient.Close(); err != nil && firstErr == nil {
		firstErr = err
	}

	return firstErr
}

// removeFromTracking removes an instance from the tracking map.
func (e *Engine) removeFromTracking(id string) {
	e.mu.Lock()
	for name, instanceID := range e.instances {
		if instanceID == id {
			delete(e.instances, name)
			break
		}
	}
	e.mu.Unlock()
}

// isNotFound reports whether err is a "not found" (404) error from the
// GCP API.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	// The google-cloud-go compute library wraps googleapi.Error.
	// Check the error string for the 404 status code pattern.
	// This is more robust than type-asserting through multiple
	// wrapping layers.
	return containsHTTP404(err)
}

// containsHTTP404 checks if the error chain contains an HTTP 404.
func containsHTTP404(err error) bool {
	// google-cloud-go wraps errors; use string matching as a pragmatic
	// approach that survives library version changes.
	errStr := err.Error()
	return contains404Pattern(errStr)
}

// contains404Pattern checks for common 404 patterns in GCP error strings.
func contains404Pattern(s string) bool {
	// googleapi.Error formats as "googleapi: Error 404: ..."
	// gRPC status formats as "code = NotFound"
	for _, pattern := range []string{
		"Error 404",
		"code = NotFound",
		"notFound",
	} {
		if containsString(s, pattern) {
			return true
		}
	}
	return false
}

// containsString is a simple substring check to avoid importing strings
// for a single call.
func containsString(s, substr string) bool {
	return len(substr) <= len(s) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
