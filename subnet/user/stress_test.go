//go:build stress

package user

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"subnet/host"
	"subnet/internal/testutil"
	"subnet/signing"
	"subnet/state"
	"subnet/stub"
	"subnet/types"
)

const (
	stressNumHosts    = 16
	stressBalance     = 10_000_000
	stressModel       = "llama-3.1-70b"
	stressInputLength = 200
	stressMaxTokens   = 100
	actualCostPerInf  = 120 // stub: (80+40)*1
)

var stressPrompt = []byte(`{"messages":[{"role":"system","content":"You are an expert assistant specializing in distributed systems and consensus algorithms."},{"role":"user","content":"Explain the trade-offs between BFT-style consensus and Nakamoto-style consensus in the context of a decentralized AI inference network. Consider latency, throughput, finality guarantees, and validator set management. Include concrete examples from existing systems."}]}`)

// ConcurrentClient wraps InProcessClient so the host processes requests in its
// own goroutine, letting the Go scheduler spread hosts across OS threads/cores.
type ConcurrentClient struct {
	inner *InProcessClient
	wg    sync.WaitGroup
}

func (c *ConcurrentClient) Send(ctx context.Context, req host.HostRequest) (*host.HostResponse, error) {
	type result struct {
		resp *host.HostResponse
		err  error
	}
	ch := make(chan result, 1)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		resp, err := c.inner.Send(ctx, req)
		ch <- result{resp, err}
	}()
	r := <-ch
	return r.resp, r.err
}

// timing collects duration samples and reports percentile stats.
type timing struct {
	label   string
	samples []time.Duration
}

func newTiming(label string, cap int) *timing {
	return &timing{label: label, samples: make([]time.Duration, 0, cap)}
}

func (tm *timing) add(d time.Duration) { tm.samples = append(tm.samples, d) }

func (tm *timing) report(t *testing.T) {
	n := len(tm.samples)
	if n == 0 {
		t.Logf("  %s: no samples", tm.label)
		return
	}
	sorted := make([]time.Duration, n)
	copy(sorted, tm.samples)
	slices.SortFunc(sorted, func(a, b time.Duration) int { return cmp.Compare(a, b) })

	var total time.Duration
	for _, d := range sorted {
		total += d
	}
	avg := total / time.Duration(n)
	p50 := sorted[n/2]
	p99idx := int(float64(n) * 0.99)
	if p99idx >= n {
		p99idx = n - 1
	}
	p99 := sorted[p99idx]
	max := sorted[n-1]

	t.Logf("  %s: n=%d avg=%v p50=%v p99=%v max=%v total=%v", tm.label, n, avg, p50, p99, max, total)
}

func measureStateSize(st types.EscrowState) (hostStatsMB, inferencesMB, totalMB float64) {
	slotIDs := make([]uint32, 0, len(st.HostStats))
	for id := range st.HostStats {
		slotIDs = append(slotIDs, id)
	}
	slices.SortFunc(slotIDs, func(a, b uint32) int { return cmp.Compare(a, b) })
	entries := make([]*types.HostStatsProto, 0, len(slotIDs))
	for _, id := range slotIDs {
		s := st.HostStats[id]
		entries = append(entries, &types.HostStatsProto{
			SlotId:               id,
			Missed:               s.Missed,
			Invalid:              s.Invalid,
			Cost:                 s.Cost,
			RequiredValidations:  s.RequiredValidations,
			CompletedValidations: s.CompletedValidations,
		})
	}
	hsData, _ := proto.Marshal(&types.HostStatsMapProto{Entries: entries})

	ids := make([]uint64, 0, len(st.Inferences))
	for id := range st.Inferences {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uint64) int { return cmp.Compare(a, b) })
	infEntries := make([]*types.InferenceRecordProto, 0, len(ids))
	for _, id := range ids {
		r := st.Inferences[id]
		var votedSlots []uint32
		for slot := range r.VotedSlots {
			votedSlots = append(votedSlots, slot)
		}
		slices.SortFunc(votedSlots, func(a, b uint32) int { return cmp.Compare(a, b) })
		infEntries = append(infEntries, &types.InferenceRecordProto{
			InferenceId:    id,
			Status:         uint32(r.Status),
			ExecutorSlot:   r.ExecutorSlot,
			Model:          r.Model,
			PromptHash:     r.PromptHash,
			ResponseHash:   r.ResponseHash,
			InputLength:    r.InputLength,
			MaxTokens:      r.MaxTokens,
			InputTokens:    r.InputTokens,
			OutputTokens:   r.OutputTokens,
			ReservedCost:   r.ReservedCost,
			ActualCost:     r.ActualCost,
			StartedAt:      r.StartedAt,
			ConfirmedAt:    r.ConfirmedAt,
			VotesValid:     r.VotesValid,
			VotesInvalid:   r.VotesInvalid,
			VotedSlots:     votedSlots,
			ValidatorSlot:  r.ValidatorSlot,
			ValidatorValid: r.ValidatorValid,
			ValidatedBy:    r.ValidatedBy,
		})
	}
	infData, _ := proto.Marshal(&types.InferencesMapProto{Entries: infEntries})

	const mb = 1024.0 * 1024.0
	hostStatsMB = float64(len(hsData)) / mb
	inferencesMB = float64(len(infData)) / mb
	totalMB = hostStatsMB + inferencesMB
	return
}

func measureDiffHistorySize(diffs []types.Diff) float64 {
	var total int
	for _, d := range diffs {
		content := &types.DiffContent{
			Nonce:         d.Nonce,
			Txs:           d.Txs,
			PostStateRoot: d.PostStateRoot,
		}
		total += proto.Size(content)
	}
	return float64(total) / (1024.0 * 1024.0)
}

func runStress(t *testing.T, rounds int) {
	numCPU := runtime.NumCPU()
	prev := runtime.GOMAXPROCS(numCPU)
	defer runtime.GOMAXPROCS(prev)

	totalInf := stressNumHosts * rounds
	grace := uint64(totalInf + 100)

	totalStart := time.Now()

	// --- Setup ---
	hostSigners := make([]*signing.Secp256k1Signer, stressNumHosts)
	for i := range hostSigners {
		hostSigners[i] = testutil.MustGenerateKey(t)
	}
	userKey := testutil.MustGenerateKey(t)
	group := testutil.MakeGroup(hostSigners)
	config := types.SessionConfig{
		RefusalTimeout:   60,
		ExecutionTimeout: 1200,
		TokenPrice:       1,
		VoteThreshold:    uint32(stressNumHosts) / 2,
		ValidationRate:   5000, // 50%
	}
	verifier := signing.NewSecp256k1Verifier()

	clients := make([]HostClient, stressNumHosts)
	for i := range hostSigners {
		sm := state.NewStateMachine("escrow-stress", config, group, stressBalance, userKey.Address(), verifier)
		engine := stub.NewInferenceEngine()
		h, err := host.NewHost(sm, hostSigners[i], engine, "escrow-stress", group, nil, host.WithGrace(grace))
		require.NoError(t, err)
		clients[i] = &ConcurrentClient{inner: &InProcessClient{Host: h}}
	}

	userSM := state.NewStateMachine("escrow-stress", config, group, stressBalance, userKey.Address(), verifier)
	session, err := NewSession(userSM, userKey, "escrow-stress", group, clients)
	require.NoError(t, err)

	ctx := context.Background()
	params := InferenceParams{
		Model:       stressModel,
		Prompt:      stressPrompt,
		InputLength: stressInputLength,
		MaxTokens:   stressMaxTokens,
		StartedAt:   1000,
	}

	// --- Inference loop ---
	infTiming := newTiming("inference_phase", totalInf)
	for i := 0; i < totalInf; i++ {
		start := time.Now()
		_, err := session.SendInference(ctx, params)
		require.NoError(t, err, "inference %d failed", i+1)
		infTiming.add(time.Since(start))
	}

	// --- State root at peak (pre-finalize, measures raw hashing cost at N inferences) ---
	preFinSt := session.StateMachine().SnapshotState()
	srStart := time.Now()
	_, err = state.ComputeStateRoot(preFinSt.Balance, preFinSt.HostStats, preFinSt.Inferences)
	require.NoError(t, err)
	srDuration := time.Since(srStart)

	// --- Finalize ---
	finStart := time.Now()
	err = session.Finalize(ctx)
	require.NoError(t, err)
	finDuration := time.Since(finStart)

	// --- Settlement ---
	st := session.StateMachine().SnapshotState()
	finalNonce := session.Nonce()
	sigs := session.Signatures()
	latestSigs, ok := sigs[finalNonce]
	require.True(t, ok, "should have signatures for final nonce %d", finalNonce)

	settleStart := time.Now()
	payload, err := state.BuildSettlement(st, latestSigs, finalNonce)
	require.NoError(t, err)
	settleDuration := time.Since(settleStart)

	totalDuration := time.Since(totalStart)

	// --- Memory report ---
	hostStatsMB, inferencesMB, totalStateMB := measureStateSize(st)
	diffHistoryMB := measureDiffHistorySize(session.Diffs())

	// --- Verification ---

	// 1. Every inference in StatusFinished.
	for id, rec := range st.Inferences {
		require.Equal(t, types.StatusFinished, rec.Status, "inference %d not finished (status=%d)", id, rec.Status)
	}

	// 2. Each host executed exactly `rounds` inferences.
	expectedCostPerHost := uint64(rounds) * actualCostPerInf
	for slot, hs := range st.HostStats {
		require.Equal(t, expectedCostPerHost, hs.Cost,
			"slot %d: cost=%d expected=%d", slot, hs.Cost, expectedCostPerHost)
	}

	// 3. All 16 seeds revealed.
	require.Len(t, st.RevealedSeeds, stressNumHosts, "expected %d revealed seeds", stressNumHosts)

	// 4. All hosts signed at the final nonce.
	require.Equal(t, stressNumHosts, len(latestSigs),
		"expected %d signatures at final nonce, got %d", stressNumHosts, len(latestSigs))
	finalSignedCount := len(latestSigs)

	// 5. Settlement Merkle: sha256(hostStatsHash || restHash) == stateRoot.
	hostStatsHash, err := state.ComputeHostStatsHash(st.HostStats)
	require.NoError(t, err)
	h := sha256.New()
	h.Write(hostStatsHash)
	h.Write(payload.RestHash)
	expectedRoot := h.Sum(nil)
	require.Equal(t, expectedRoot, payload.StateRoot, "Merkle proof mismatch")

	// 6. Balance: initialBalance - totalInf * actualCost == st.Balance.
	expectedBalance := uint64(stressBalance) - uint64(totalInf)*actualCostPerInf
	require.Equal(t, expectedBalance, st.Balance,
		"balance: got %d expected %d", st.Balance, expectedBalance)

	// --- Report ---
	numDiffs := len(session.Diffs())
	expectedDiffs := totalInf + stressNumHosts + 1

	t.Logf("")
	t.Logf("--- stress test report ---")
	t.Logf("config: hosts=%d rounds=%d inferences=%d GOMAXPROCS=%d NumCPU=%d",
		stressNumHosts, rounds, totalInf, numCPU, numCPU)
	t.Logf("")
	t.Logf("timing:")
	infTiming.report(t)
	t.Logf("  state_root_at_N=%d: %v", totalInf, srDuration)
	t.Logf("  finalize_phase: %v (%d diffs, %d seed reveals)", finDuration, stressNumHosts+1, stressNumHosts)
	t.Logf("  settlement: %v", settleDuration)
	t.Logf("  total: %v", totalDuration)
	t.Logf("")
	t.Logf("memory:")
	t.Logf("  state_size: %.2f MB (host_stats=%.2f KB, inferences=%.2f MB)",
		totalStateMB, hostStatsMB*1024, inferencesMB)
	t.Logf("  diff_history: %.2f MB (%d diffs)", diffHistoryMB, numDiffs)
	t.Logf("")
	t.Logf("signatures:")
	t.Logf("  final_state: %d/%d signatures collected (nonce=%d)",
		finalSignedCount, stressNumHosts, finalNonce)
	t.Logf("")
	t.Logf("correctness:")
	t.Logf("  final_balance: %d (expected %d)", st.Balance, expectedBalance)
	t.Logf("  revealed_seeds: %d/%d", len(st.RevealedSeeds), stressNumHosts)
	t.Logf("  diffs: %d (expected %d)", numDiffs, expectedDiffs)
	t.Logf("  host_stats:")
	for slot := uint32(0); slot < stressNumHosts; slot++ {
		hs := st.HostStats[slot]
		t.Logf("    slot %d: cost=%d missed=%d invalid=%d req_val=%d comp_val=%d",
			slot, hs.Cost, hs.Missed, hs.Invalid, hs.RequiredValidations, hs.CompletedValidations)
	}

	_ = binary.BigEndian // suppress unused import
}

func TestStress(t *testing.T) {
	t.Run("3_rounds", func(t *testing.T) { runStress(t, 3) })
	t.Run("10_rounds", func(t *testing.T) { runStress(t, 10) })
	t.Run("30_rounds", func(t *testing.T) { runStress(t, 30) })
}
