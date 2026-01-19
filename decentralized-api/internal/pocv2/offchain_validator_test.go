package pocv2

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSampleLeafIndices_AllIndices(t *testing.T) {
	// When sampleSize >= count, return all indices
	indices := sampleLeafIndices("pubkey", "blockhash", 1000, 5, 10)

	assert.Len(t, indices, 5)
	// Should be 0, 1, 2, 3, 4
	for i := uint32(0); i < 5; i++ {
		assert.Contains(t, indices, i)
	}
}

func TestSampleLeafIndices_SampleSubset(t *testing.T) {
	count := uint32(100)
	sampleSize := 10

	indices := sampleLeafIndices("pubkey", "blockhash", 1000, count, sampleSize)

	assert.Len(t, indices, sampleSize)

	// All indices should be valid (< count)
	for _, idx := range indices {
		assert.Less(t, idx, count)
	}

	// All indices should be unique
	seen := make(map[uint32]bool)
	for _, idx := range indices {
		assert.False(t, seen[idx], "duplicate index found: %d", idx)
		seen[idx] = true
	}
}

func TestSampleLeafIndices_Deterministic(t *testing.T) {
	count := uint32(1000)
	sampleSize := 50

	// Same inputs should produce same output
	indices1 := sampleLeafIndices("pubkey", "blockhash", 1000, count, sampleSize)
	indices2 := sampleLeafIndices("pubkey", "blockhash", 1000, count, sampleSize)

	assert.Equal(t, indices1, indices2)
}

func TestSampleLeafIndices_DifferentInputs(t *testing.T) {
	count := uint32(1000)
	sampleSize := 50

	// Different inputs should produce different outputs
	indices1 := sampleLeafIndices("pubkey1", "blockhash", 1000, count, sampleSize)
	indices2 := sampleLeafIndices("pubkey2", "blockhash", 1000, count, sampleSize)
	indices3 := sampleLeafIndices("pubkey1", "blockhash2", 1000, count, sampleSize)
	indices4 := sampleLeafIndices("pubkey1", "blockhash", 2000, count, sampleSize)

	// At least some indices should differ
	assert.NotEqual(t, indices1, indices2)
	assert.NotEqual(t, indices1, indices3)
	assert.NotEqual(t, indices1, indices4)
}

func TestSampleLeafIndices_ZeroCount(t *testing.T) {
	indices := sampleLeafIndices("pubkey", "blockhash", 1000, 0, 10)
	assert.Nil(t, indices)
}

func TestSampleLeafIndices_Distribution(t *testing.T) {
	// Test that sampling is reasonably distributed
	count := uint32(100)
	sampleSize := 20

	// Run multiple samplings with different inputs
	allSampled := make(map[uint32]int)
	for i := 0; i < 50; i++ {
		indices := sampleLeafIndices("pubkey", "blockhash", int64(1000+i), count, sampleSize)
		for _, idx := range indices {
			allSampled[idx]++
		}
	}

	// Most indices should have been sampled at least once
	sampledCount := 0
	for idx := uint32(0); idx < count; idx++ {
		if allSampled[idx] > 0 {
			sampledCount++
		}
	}
	// With 50 samplings of 20 from 100, we should cover most indices
	assert.Greater(t, sampledCount, 80, "sampling should cover most indices")
}

func TestDefaultValidationConfig(t *testing.T) {
	config := DefaultValidationConfig()

	assert.Equal(t, 10, config.WorkerCount)
	assert.Greater(t, config.RequestTimeout.Seconds(), float64(0))
	assert.Greater(t, config.MaxRetries, 0)
	assert.Greater(t, config.RetryBackoff.Seconds(), float64(0))
}
