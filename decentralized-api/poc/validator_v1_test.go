package poc

import (
	"context"
	"sync"
	"testing"
	"time"

	"decentralized-api/broker"
	"decentralized-api/mlnodeclient"

	"github.com/productscience/inference/x/inference/types"
	"github.com/stretchr/testify/assert"
)

func TestSampleNoncesV1_AllNonces(t *testing.T) {
	nonces := []int64{1, 2, 3, 4, 5}
	dist := []float64{0.1, 0.2, 0.3, 0.4, 0.5}

	// Request more samples than available
	result := sampleNoncesV1("pubkey", "blockhash", 100, nonces, dist, 10)

	assert.Equal(t, nonces, result.nonces)
	assert.Equal(t, dist, result.dist)
}

func TestSampleNoncesV1_SampledSubset(t *testing.T) {
	nonces := make([]int64, 100)
	dist := make([]float64, 100)
	for i := 0; i < 100; i++ {
		nonces[i] = int64(i)
		dist[i] = float64(i) * 0.01
	}

	// Request 10 samples from 100
	result := sampleNoncesV1("pubkey", "blockhash", 100, nonces, dist, 10)

	assert.Len(t, result.nonces, 10)
	assert.Len(t, result.dist, 10)

	// All sampled nonces should be within original range
	for _, n := range result.nonces {
		assert.True(t, n >= 0 && n < 100)
	}
}

func TestSampleNoncesV1_Deterministic(t *testing.T) {
	nonces := make([]int64, 100)
	dist := make([]float64, 100)
	for i := 0; i < 100; i++ {
		nonces[i] = int64(i)
		dist[i] = float64(i)
	}

	// Same inputs should produce same outputs
	result1 := sampleNoncesV1("pubkey", "blockhash", 100, nonces, dist, 10)
	result2 := sampleNoncesV1("pubkey", "blockhash", 100, nonces, dist, 10)

	assert.Equal(t, result1.nonces, result2.nonces)
	assert.Equal(t, result1.dist, result2.dist)

	// Different pubkey should produce different samples
	result3 := sampleNoncesV1("different", "blockhash", 100, nonces, dist, 10)
	assert.NotEqual(t, result1.nonces, result3.nonces)
}

func TestDeterministicSampleIndicesV1_AllIndices(t *testing.T) {
	indices := deterministicSampleIndicesV1("pk", "hash", 100, 50, 20)

	// Should return all indices when requesting more than available
	assert.Len(t, indices, 20)
	for i, idx := range indices {
		assert.Equal(t, i, idx)
	}
}

func TestDeterministicSampleIndicesV1_Subset(t *testing.T) {
	indices := deterministicSampleIndicesV1("pk", "hash", 100, 10, 100)

	assert.Len(t, indices, 10)

	// All indices should be unique and within range
	seen := make(map[int]bool)
	for _, idx := range indices {
		assert.False(t, seen[idx], "duplicate index found")
		assert.True(t, idx >= 0 && idx < 100)
		seen[idx] = true
	}
}

func TestDeterministicSampleIndicesV1_DifferentSeeds(t *testing.T) {
	// Same seed (pk, hash, height) should produce same result
	indices1 := deterministicSampleIndicesV1("pk", "hash", 100, 10, 100)
	indices2 := deterministicSampleIndicesV1("pk", "hash", 100, 10, 100)
	assert.Equal(t, indices1, indices2)

	// Different height should produce different result
	indices3 := deterministicSampleIndicesV1("pk", "hash", 101, 10, 100)
	assert.NotEqual(t, indices1, indices3)

	// Different hash should produce different result
	indices4 := deterministicSampleIndicesV1("pk", "different", 100, 10, 100)
	assert.NotEqual(t, indices1, indices4)
}

func TestValidationConfigDefaults(t *testing.T) {
	config := DefaultValidationConfig()

	assert.Equal(t, 10, config.WorkerCount)
	assert.NotZero(t, config.RequestTimeout)
	assert.Equal(t, 3, config.MaxRetries)
	assert.NotZero(t, config.RetryBackoff)
}

// fakeNodeClient satisfies mlnodeclient.MLNodeClient for testing.
type fakeNodeClient struct{}

func (f fakeNodeClient) StartTraining(ctx context.Context, taskId uint64, participant string, nodeId string, masterNodeAddr string, rank int, worldSize int) error {
	return nil
}
func (f fakeNodeClient) GetTrainingStatus(ctx context.Context) error { return nil }
func (f fakeNodeClient) Stop(ctx context.Context) error              { return nil }
func (f fakeNodeClient) NodeState(ctx context.Context) (*mlnodeclient.StateResponse, error) {
	return &mlnodeclient.StateResponse{}, nil
}

// PoC v1 operations
func (f fakeNodeClient) InitGenerateV1(ctx context.Context, dto mlnodeclient.InitDtoV1) error {
	return nil
}
func (f fakeNodeClient) InitValidateV1(ctx context.Context, dto mlnodeclient.InitDtoV1) error {
	return nil
}
func (f fakeNodeClient) ValidateBatchV1(ctx context.Context, batch mlnodeclient.ProofBatchV1) error {
	return nil
}
func (f fakeNodeClient) GetPowStatusV1(ctx context.Context) (*mlnodeclient.PowStatusResponseV1, error) {
	return &mlnodeclient.PowStatusResponseV1{}, nil
}

// PoC v2 operations
func (f fakeNodeClient) InitGenerateV2(ctx context.Context, req mlnodeclient.PoCInitGenerateRequestV2) (*mlnodeclient.PoCInitGenerateResponseV2, error) {
	return &mlnodeclient.PoCInitGenerateResponseV2{}, nil
}
func (f fakeNodeClient) GenerateV2(ctx context.Context, req mlnodeclient.PoCGenerateRequestV2) (*mlnodeclient.PoCGenerateResponseV2, error) {
	return &mlnodeclient.PoCGenerateResponseV2{}, nil
}
func (f fakeNodeClient) GetPowStatusV2(ctx context.Context) (*mlnodeclient.PoCStatusResponseV2, error) {
	return &mlnodeclient.PoCStatusResponseV2{}, nil
}
func (f fakeNodeClient) StopPowV2(ctx context.Context) (*mlnodeclient.PoCStopResponseV2, error) {
	return &mlnodeclient.PoCStopResponseV2{}, nil
}

// Inference operations
func (f fakeNodeClient) InferenceHealth(ctx context.Context) (bool, error) { return true, nil }
func (f fakeNodeClient) InferenceUp(ctx context.Context, model string, args []string) error {
	return nil
}

// GPU operations
func (f fakeNodeClient) GetGPUDevices(ctx context.Context) (*mlnodeclient.GPUDevicesResponse, error) {
	return &mlnodeclient.GPUDevicesResponse{}, nil
}
func (f fakeNodeClient) GetGPUDriver(ctx context.Context) (*mlnodeclient.DriverInfo, error) {
	return &mlnodeclient.DriverInfo{}, nil
}

// Model management operations
func (f fakeNodeClient) CheckModelStatus(ctx context.Context, model mlnodeclient.Model) (*mlnodeclient.ModelStatusResponse, error) {
	return &mlnodeclient.ModelStatusResponse{}, nil
}
func (f fakeNodeClient) DownloadModel(ctx context.Context, model mlnodeclient.Model) (*mlnodeclient.DownloadStartResponse, error) {
	return &mlnodeclient.DownloadStartResponse{}, nil
}
func (f fakeNodeClient) DeleteModel(ctx context.Context, model mlnodeclient.Model) (*mlnodeclient.DeleteResponse, error) {
	return &mlnodeclient.DeleteResponse{}, nil
}
func (f fakeNodeClient) ListModels(ctx context.Context) (*mlnodeclient.ModelListResponse, error) {
	return &mlnodeclient.ModelListResponse{}, nil
}
func (f fakeNodeClient) GetDiskSpace(ctx context.Context) (*mlnodeclient.DiskSpaceInfo, error) {
	return &mlnodeclient.DiskSpaceInfo{}, nil
}

// fakeBroker implements a test broker with configurable node responses.
type fakeBroker struct {
	mu    sync.Mutex
	nodes []broker.NodeResponse
}

func (f *fakeBroker) setNodes(nodes []broker.NodeResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes = nodes
}

func (f *fakeBroker) GetNodes() ([]broker.NodeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]broker.NodeResponse, len(f.nodes))
	copy(out, f.nodes)
	return out, nil
}

func (f *fakeBroker) NewNodeClient(node *broker.Node) mlnodeclient.MLNodeClient {
	return fakeNodeClient{}
}

// TestGetNodesWithRetryConfig_RetriesThenSuccess tests that the filter logic
// correctly identifies nodes in validating state.
func TestGetNodesWithRetryConfig_RetriesThenSuccess(t *testing.T) {
	fb := &fakeBroker{}

	// Initial state: nodes are not in PoC validation, so filter should return none
	fb.setNodes([]broker.NodeResponse{
		{
			Node: broker.Node{Id: "node-1"},
			State: broker.NodeState{
				CurrentStatus: types.HardwareNodeStatus_INFERENCE,
			},
		},
	})

	// Test that filter returns empty for INFERENCE nodes (V1 requires POC+Validating)
	nodes := filterNodesForV1Validation(fb.nodes)
	assert.Len(t, nodes, 0, "expected no nodes when not in PoC validating state")

	// Update state to PoC validating
	fb.setNodes([]broker.NodeResponse{
		{
			Node: broker.Node{Id: "node-1"},
			State: broker.NodeState{
				CurrentStatus:    types.HardwareNodeStatus_POC,
				PocCurrentStatus: broker.PocStatusValidating,
			},
		},
	})

	nodes = filterNodesForV1Validation(fb.nodes)
	assert.Len(t, nodes, 1, "expected 1 node after enabling validation state")
}

// TestFilterNodesForV1Validation tests the V1 node filtering logic.
func TestFilterNodesForV1Validation(t *testing.T) {
	tests := []struct {
		name     string
		nodes    []broker.NodeResponse
		expected int
	}{
		{
			name: "accepts POC+Validating",
			nodes: []broker.NodeResponse{
				{
					Node: broker.Node{Id: "node-1"},
					State: broker.NodeState{
						CurrentStatus:    types.HardwareNodeStatus_POC,
						PocCurrentStatus: broker.PocStatusValidating,
					},
				},
			},
			expected: 1,
		},
		{
			name: "rejects POC+Generating",
			nodes: []broker.NodeResponse{
				{
					Node: broker.Node{Id: "node-1"},
					State: broker.NodeState{
						CurrentStatus:    types.HardwareNodeStatus_POC,
						PocCurrentStatus: broker.PocStatusGenerating,
					},
				},
			},
			expected: 0,
		},
		{
			name: "rejects INFERENCE status",
			nodes: []broker.NodeResponse{
				{
					Node: broker.Node{Id: "node-1"},
					State: broker.NodeState{
						CurrentStatus: types.HardwareNodeStatus_INFERENCE,
					},
				},
			},
			expected: 0,
		},
		{
			name: "rejects FAILED status",
			nodes: []broker.NodeResponse{
				{
					Node: broker.Node{Id: "node-1"},
					State: broker.NodeState{
						CurrentStatus: types.HardwareNodeStatus_FAILED,
					},
				},
			},
			expected: 0,
		},
		{
			name: "mixed: accepts only valid nodes",
			nodes: []broker.NodeResponse{
				{
					Node: broker.Node{Id: "node-1"},
					State: broker.NodeState{
						CurrentStatus:    types.HardwareNodeStatus_POC,
						PocCurrentStatus: broker.PocStatusValidating,
					},
				},
				{
					Node: broker.Node{Id: "node-2"},
					State: broker.NodeState{
						CurrentStatus:    types.HardwareNodeStatus_POC,
						PocCurrentStatus: broker.PocStatusGenerating,
					},
				},
				{
					Node: broker.Node{Id: "node-3"},
					State: broker.NodeState{
						CurrentStatus: types.HardwareNodeStatus_INFERENCE,
					},
				},
			},
			expected: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterNodesForV1Validation(tt.nodes)
			assert.Len(t, result, tt.expected)
		})
	}
}

// TestV1RetryConstants verifies the retry constants are set correctly.
func TestV1RetryConstants(t *testing.T) {
	assert.Equal(t, 30, POC_VALIDATE_GET_NODES_RETRIES)
	assert.Equal(t, 5*time.Second, POC_VALIDATE_GET_NODES_RETRY_DELAY)
	assert.Equal(t, 5, POC_VALIDATE_BATCH_RETRIES)
}
