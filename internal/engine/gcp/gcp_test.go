package gcp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"

	computepb "cloud.google.com/go/compute/apiv1/computepb"
	gax "github.com/googleapis/gax-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

// ---------------------------------------------------------------------------
// Mock operation (satisfies operationWaiter)
// ---------------------------------------------------------------------------

type mockOperation struct {
	err error
}

func (m *mockOperation) Wait(_ context.Context, _ ...gax.CallOption) error {
	return m.err
}

// ---------------------------------------------------------------------------
// Mock instances client (satisfies instancesAPI)
// ---------------------------------------------------------------------------

type mockInstancesClient struct {
	mu sync.Mutex

	insertCalls []*computepb.InsertInstanceRequest
	deleteCalls []*computepb.DeleteInstanceRequest
	closed      bool

	insertErr error // returned by Insert
	insertOp  operationWaiter
	deleteErr error // returned by Delete
	deleteOp  operationWaiter
}

func newMockInstancesClient() *mockInstancesClient {
	return &mockInstancesClient{
		insertOp: &mockOperation{},
		deleteOp: &mockOperation{},
	}
}

func (m *mockInstancesClient) Insert(_ context.Context, req *computepb.InsertInstanceRequest) (operationWaiter, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.insertCalls = append(m.insertCalls, req)
	if m.insertErr != nil {
		return nil, m.insertErr
	}
	return m.insertOp, nil
}

func (m *mockInstancesClient) Delete(_ context.Context, req *computepb.DeleteInstanceRequest) (operationWaiter, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.deleteCalls = append(m.deleteCalls, req)
	if m.deleteErr != nil {
		return nil, m.deleteErr
	}
	return m.deleteOp, nil
}

func (m *mockInstancesClient) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

// ---------------------------------------------------------------------------
// Mock closer (satisfies closerOnly for opClient)
// ---------------------------------------------------------------------------

type mockCloser struct {
	closed bool
}

func (m *mockCloser) Close() error {
	m.closed = true
	return nil
}

// ---------------------------------------------------------------------------
// Test suite
// ---------------------------------------------------------------------------

type GCPEngineSuite struct {
	suite.Suite
	ctx      context.Context
	client   *mockInstancesClient
	opCloser *mockCloser
	logger   *slog.Logger
	cfg      Config
}

func (s *GCPEngineSuite) SetupTest() {
	s.ctx = context.Background()
	s.client = newMockInstancesClient()
	s.opCloser = &mockCloser{}
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	s.cfg = Config{
		Project:     "test-project",
		Zone:        "us-central1-a",
		MachineType: "e2-medium",
		Image:       "projects/test-project/global/images/runner-image",
		DiskSizeGB:  50,
		Network:     "default",
		PublicIP:    true,
	}
}

func (s *GCPEngineSuite) newEngine() *Engine {
	return newEngine(s.client, s.opCloser, s.cfg, s.logger)
}

func TestGCPEngineSuite(t *testing.T) {
	suite.Run(t, new(GCPEngineSuite))
}

// ---------------------------------------------------------------------------
// StartRunner tests
// ---------------------------------------------------------------------------

func (s *GCPEngineSuite) TestStartRunner_Success() {
	e := s.newEngine()

	id, err := e.StartRunner(s.ctx, "runner-abc123", "base64-jit-config")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "runner-abc123", id) // GCP uses instance name as ID

	// Verify instance is tracked
	e.mu.Lock()
	assert.Contains(s.T(), e.instances, "runner-abc123")
	e.mu.Unlock()

	// Verify the Insert request was well-formed
	require.Len(s.T(), s.client.insertCalls, 1)
	req := s.client.insertCalls[0]
	assert.Equal(s.T(), "test-project", req.GetProject())
	assert.Equal(s.T(), "us-central1-a", req.GetZone())

	inst := req.GetInstanceResource()
	assert.Equal(s.T(), "runner-abc123", inst.GetName())
	assert.Contains(s.T(), inst.GetMachineType(), "e2-medium")

	// Verify JIT config is in metadata
	var foundJit bool
	for _, item := range inst.GetMetadata().GetItems() {
		if item.GetKey() == "ACTIONS_RUNNER_INPUT_JITCONFIG" {
			assert.Equal(s.T(), "base64-jit-config", item.GetValue())
			foundJit = true
		}
	}
	assert.True(s.T(), foundJit, "JIT config should be in instance metadata")
}

func (s *GCPEngineSuite) TestStartRunner_DiskConfig() {
	s.cfg.DiskSizeGB = 100
	e := s.newEngine()

	_, err := e.StartRunner(s.ctx, "runner-disk", "jit")
	require.NoError(s.T(), err)

	inst := s.client.insertCalls[0].GetInstanceResource()
	require.Len(s.T(), inst.GetDisks(), 1)
	disk := inst.GetDisks()[0]
	assert.True(s.T(), disk.GetAutoDelete())
	assert.True(s.T(), disk.GetBoot())
	assert.Equal(s.T(), int64(100), disk.GetInitializeParams().GetDiskSizeGb())
	assert.Equal(s.T(), s.cfg.Image, disk.GetInitializeParams().GetSourceImage())
	assert.Contains(s.T(), disk.GetInitializeParams().GetDiskType(), "pd-ssd")
}

func (s *GCPEngineSuite) TestStartRunner_PublicIP() {
	s.cfg.PublicIP = true
	e := s.newEngine()

	_, err := e.StartRunner(s.ctx, "runner-pub", "jit")
	require.NoError(s.T(), err)

	inst := s.client.insertCalls[0].GetInstanceResource()
	require.Len(s.T(), inst.GetNetworkInterfaces(), 1)
	nic := inst.GetNetworkInterfaces()[0]
	assert.Len(s.T(), nic.GetAccessConfigs(), 1, "should have access config for public IP")
}

func (s *GCPEngineSuite) TestStartRunner_NoPublicIP() {
	s.cfg.PublicIP = false
	e := s.newEngine()

	_, err := e.StartRunner(s.ctx, "runner-priv", "jit")
	require.NoError(s.T(), err)

	inst := s.client.insertCalls[0].GetInstanceResource()
	nic := inst.GetNetworkInterfaces()[0]
	assert.Empty(s.T(), nic.GetAccessConfigs(), "should have no access configs without public IP")
}

func (s *GCPEngineSuite) TestStartRunner_CustomSubnet() {
	s.cfg.Subnet = "projects/test-project/regions/us-central1/subnetworks/my-subnet"
	e := s.newEngine()

	_, err := e.StartRunner(s.ctx, "runner-subnet", "jit")
	require.NoError(s.T(), err)

	inst := s.client.insertCalls[0].GetInstanceResource()
	nic := inst.GetNetworkInterfaces()[0]
	assert.Equal(s.T(), s.cfg.Subnet, nic.GetSubnetwork())
}

func (s *GCPEngineSuite) TestStartRunner_ServiceAccount() {
	s.cfg.ServiceAccount = "runner@test-project.iam.gserviceaccount.com"
	e := s.newEngine()

	_, err := e.StartRunner(s.ctx, "runner-sa", "jit")
	require.NoError(s.T(), err)

	inst := s.client.insertCalls[0].GetInstanceResource()
	require.Len(s.T(), inst.GetServiceAccounts(), 1)
	sa := inst.GetServiceAccounts()[0]
	assert.Equal(s.T(), "runner@test-project.iam.gserviceaccount.com", sa.GetEmail())
	assert.Contains(s.T(), sa.GetScopes(), "https://www.googleapis.com/auth/cloud-platform")
}

func (s *GCPEngineSuite) TestStartRunner_NoServiceAccount() {
	s.cfg.ServiceAccount = ""
	e := s.newEngine()

	_, err := e.StartRunner(s.ctx, "runner-nosa", "jit")
	require.NoError(s.T(), err)

	inst := s.client.insertCalls[0].GetInstanceResource()
	assert.Empty(s.T(), inst.GetServiceAccounts())
}

func (s *GCPEngineSuite) TestStartRunner_InsertError() {
	s.client.insertErr = fmt.Errorf("quota exceeded")
	e := s.newEngine()

	_, err := e.StartRunner(s.ctx, "runner-fail", "jit")
	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "quota exceeded")

	// Instance should not be tracked
	e.mu.Lock()
	assert.NotContains(s.T(), e.instances, "runner-fail")
	e.mu.Unlock()
}

func (s *GCPEngineSuite) TestStartRunner_OperationWaitError() {
	s.client.insertOp = &mockOperation{err: fmt.Errorf("operation timed out")}
	e := s.newEngine()

	_, err := e.StartRunner(s.ctx, "runner-timeout", "jit")
	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "operation timed out")
}

// ---------------------------------------------------------------------------
// DestroyRunner tests
// ---------------------------------------------------------------------------

func (s *GCPEngineSuite) TestDestroyRunner_Success() {
	e := s.newEngine()

	// First start a runner so it's tracked
	_, err := e.StartRunner(s.ctx, "runner-destroy", "jit")
	require.NoError(s.T(), err)

	err = e.DestroyRunner(s.ctx, "runner-destroy")
	require.NoError(s.T(), err)

	// Verify Delete was called with correct params
	require.Len(s.T(), s.client.deleteCalls, 1)
	req := s.client.deleteCalls[0]
	assert.Equal(s.T(), "test-project", req.GetProject())
	assert.Equal(s.T(), "us-central1-a", req.GetZone())
	assert.Equal(s.T(), "runner-destroy", req.GetInstance())

	// Instance should be removed from tracking
	e.mu.Lock()
	assert.NotContains(s.T(), e.instances, "runner-destroy")
	e.mu.Unlock()
}

func (s *GCPEngineSuite) TestDestroyRunner_Idempotent_DeleteReturns404() {
	s.client.deleteErr = fmt.Errorf("googleapi: Error 404: The resource was not found")
	e := s.newEngine()

	// Manually add to tracking
	e.mu.Lock()
	e.instances["runner-gone"] = "runner-gone"
	e.mu.Unlock()

	err := e.DestroyRunner(s.ctx, "runner-gone")
	require.NoError(s.T(), err, "404 on Delete should be treated as success")

	// Should be removed from tracking
	e.mu.Lock()
	assert.NotContains(s.T(), e.instances, "runner-gone")
	e.mu.Unlock()
}

func (s *GCPEngineSuite) TestDestroyRunner_Idempotent_WaitReturns404() {
	s.client.deleteOp = &mockOperation{err: fmt.Errorf("code = NotFound")}
	e := s.newEngine()

	e.mu.Lock()
	e.instances["runner-race"] = "runner-race"
	e.mu.Unlock()

	err := e.DestroyRunner(s.ctx, "runner-race")
	require.NoError(s.T(), err, "404 during Wait should be treated as success")
}

func (s *GCPEngineSuite) TestDestroyRunner_RealError() {
	s.client.deleteErr = fmt.Errorf("permission denied: insufficient IAM permissions")
	e := s.newEngine()

	err := e.DestroyRunner(s.ctx, "runner-perms")
	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "permission denied")
}

// ---------------------------------------------------------------------------
// Shutdown tests
// ---------------------------------------------------------------------------

func (s *GCPEngineSuite) TestShutdown_DestroysAllTracked() {
	e := s.newEngine()

	// Start 3 runners
	for i := range 3 {
		_, err := e.StartRunner(s.ctx, fmt.Sprintf("runner-%d", i), "jit")
		require.NoError(s.T(), err)
	}
	assert.Len(s.T(), s.client.insertCalls, 3)

	err := e.Shutdown(s.ctx)
	require.NoError(s.T(), err)

	// All 3 should have been deleted
	assert.Len(s.T(), s.client.deleteCalls, 3)

	// Tracking map should be empty
	e.mu.Lock()
	assert.Empty(s.T(), e.instances)
	e.mu.Unlock()

	// Clients should be closed
	assert.True(s.T(), s.client.closed)
	assert.True(s.T(), s.opCloser.closed)
}

func (s *GCPEngineSuite) TestShutdown_EmptyIsClean() {
	e := s.newEngine()

	err := e.Shutdown(s.ctx)
	require.NoError(s.T(), err)

	assert.Empty(s.T(), s.client.deleteCalls)
	assert.True(s.T(), s.client.closed)
}

func (s *GCPEngineSuite) TestShutdown_PartialFailure() {
	e := s.newEngine()

	// Start 2 runners
	_, err := e.StartRunner(s.ctx, "runner-ok", "jit")
	require.NoError(s.T(), err)
	_, err = e.StartRunner(s.ctx, "runner-fail", "jit")
	require.NoError(s.T(), err)

	// Make Delete fail
	s.client.deleteErr = fmt.Errorf("network error")

	err = e.Shutdown(s.ctx)
	assert.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "network error")

	// Tracking should still be cleared
	e.mu.Lock()
	assert.Empty(s.T(), e.instances)
	e.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Instance tracking tests
// ---------------------------------------------------------------------------

func (s *GCPEngineSuite) TestTracking_MultipleRunners() {
	e := s.newEngine()

	names := []string{"runner-a", "runner-b", "runner-c"}
	for _, name := range names {
		_, err := e.StartRunner(s.ctx, name, "jit")
		require.NoError(s.T(), err)
	}

	e.mu.Lock()
	assert.Len(s.T(), e.instances, 3)
	for _, name := range names {
		assert.Contains(s.T(), e.instances, name)
	}
	e.mu.Unlock()

	// Destroy one
	err := e.DestroyRunner(s.ctx, "runner-b")
	require.NoError(s.T(), err)

	e.mu.Lock()
	assert.Len(s.T(), e.instances, 2)
	assert.NotContains(s.T(), e.instances, "runner-b")
	e.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Helper function tests
// ---------------------------------------------------------------------------

func (s *GCPEngineSuite) TestIsNotFound_Nil() {
	assert.False(s.T(), isNotFound(nil))
}

func (s *GCPEngineSuite) TestIsNotFound_GoogleAPIError() {
	err := fmt.Errorf("googleapi: Error 404: The resource was not found")
	assert.True(s.T(), isNotFound(err))
}

func (s *GCPEngineSuite) TestIsNotFound_GRPCNotFound() {
	err := fmt.Errorf("rpc error: code = NotFound desc = instance not found")
	assert.True(s.T(), isNotFound(err))
}

func (s *GCPEngineSuite) TestIsNotFound_NotFoundLower() {
	err := fmt.Errorf("some error with notFound in the message")
	assert.True(s.T(), isNotFound(err))
}

func (s *GCPEngineSuite) TestIsNotFound_OtherError() {
	err := fmt.Errorf("permission denied: insufficient IAM permissions")
	assert.False(s.T(), isNotFound(err))
}

func (s *GCPEngineSuite) TestContains404Pattern() {
	assert.True(s.T(), contains404Pattern("googleapi: Error 404: not found"))
	assert.True(s.T(), contains404Pattern("code = NotFound"))
	assert.True(s.T(), contains404Pattern("resource notFound"))
	assert.False(s.T(), contains404Pattern("Error 500: internal server error"))
	assert.False(s.T(), contains404Pattern("everything is fine"))
}

// ---------------------------------------------------------------------------
// Default config tests
// ---------------------------------------------------------------------------

func (s *GCPEngineSuite) TestNewEngine_DefaultConfig() {
	cfg := Config{
		Project: "p",
		Zone:    "z",
		Image:   "img",
	}
	// newEngine applies no defaults -- those are in New().  But we test
	// that the constructor doesn't panic with zero values.
	e := newEngine(s.client, s.opCloser, cfg, s.logger)
	assert.NotNil(s.T(), e)
	assert.Equal(s.T(), "p", e.cfg.Project)
}
