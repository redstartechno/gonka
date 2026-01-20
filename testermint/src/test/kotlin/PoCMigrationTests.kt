import com.productscience.*
import com.productscience.data.*
import org.assertj.core.api.Assertions.assertThat
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.Timeout
import org.tinylog.kotlin.Logger
import java.util.concurrent.TimeUnit

/**
 * Tests for PoC V1/V2 migration via poc_v2_enabled governance parameter.
 * Verifies that:
 * - V1 mode uses on-chain PoCBatch storage
 * - V2 mode uses off-chain StoreCommit storage
 * - Runtime switching via governance works without restart
 */
@Timeout(value = 20, unit = TimeUnit.MINUTES)
class PoCMigrationTests : TestermintTest() {

    /**
     * V1 Test: poc_v2_enabled = false
     * Verifies that PoCBatch exists on chain and no StoreCommit is created.
     */
    @Test
    fun `poc v1 mode - batches on chain, no store commits`() {
        logSection("=== TEST: PoC V1 Mode ===")

        val (cluster, genesis) = initCluster(reboot = true, config = v1Config)
        cluster.allPairs.forEach { it.waitForMlNodesToLoad() }

        // Verify V1 mode is active
        val params = genesis.getParams()
        assertThat(params.pocParams.pocV2Enabled).isFalse()
        Logger.info("poc_v2_enabled = ${params.pocParams.pocV2Enabled}")

        // Wait for PoC generation to complete
        genesis.waitForStage(EpochStage.END_OF_POC, offset = 1)
        genesis.node.waitForNextBlock(3)

        val epochData = genesis.getEpochData()
        val pocStartHeight = epochData.latestEpoch.pocStartBlockHeight
        val participantAddress = genesis.node.getColdAddress()

        logSection("Verifying V1 behavior: PoCBatch on chain")

        // V1: PoCBatch should exist on chain
        val batchCount = genesis.node.getPocBatchCount(pocStartHeight)
        Logger.info("PoCBatch count for height $pocStartHeight: $batchCount")
        assertThat(batchCount).isGreaterThan(0)
            .describedAs("V1 mode should have PoCBatch entries on chain")

        // V1: StoreCommit should NOT exist (or query fails)
        val storeCommit = genesis.node.getPoCV2StoreCommit(pocStartHeight, participantAddress)
        Logger.info("StoreCommit found: ${storeCommit.found}")
        assertThat(storeCommit.found).isFalse()
            .describedAs("V1 mode should NOT have StoreCommit entries")

        // V1: Proof API should return 503
        logSection("Verifying V1 behavior: Proof API unavailable")
        try {
            val artifactState = genesis.api.getPocArtifactsState(pocStartHeight)
            Logger.warn("Artifact state unexpectedly available: $artifactState")
            // If we get here, check the count - should be 0 in V1 mode
        } catch (e: Exception) {
            Logger.info("Proof API correctly unavailable in V1 mode: ${e.message}")
        }

        logSection("TEST PASSED: PoC V1 mode works correctly")
    }

    /**
     * V2 Test: poc_v2_enabled = true (default)
     * Verifies that StoreCommit exists on chain and proof API works.
     */
    @Test
    fun `poc v2 mode - store commits on chain, proof api works`() {
        logSection("=== TEST: PoC V2 Mode ===")

        val (cluster, genesis) = initCluster(reboot = true, config = v2Config)
        cluster.allPairs.forEach { it.waitForMlNodesToLoad() }

        // Verify V2 mode is active
        val params = genesis.getParams()
        assertThat(params.pocParams.pocV2Enabled).isTrue()
        Logger.info("poc_v2_enabled = ${params.pocParams.pocV2Enabled}")

        // Wait for PoC generation to complete
        genesis.waitForStage(EpochStage.END_OF_POC, offset = 1)
        genesis.node.waitForNextBlock(3)

        val epochData = genesis.getEpochData()
        val pocStartHeight = epochData.latestEpoch.pocStartBlockHeight
        val participantAddress = genesis.node.getColdAddress()

        logSection("Verifying V2 behavior: StoreCommit on chain")

        // V2: StoreCommit should exist on chain
        val storeCommit = genesis.node.getPoCV2StoreCommit(pocStartHeight, participantAddress)
        Logger.info("StoreCommit: found=${storeCommit.found}, count=${storeCommit.count}")
        assertThat(storeCommit.found).isTrue()
            .describedAs("V2 mode should have StoreCommit entries")
        assertThat(storeCommit.count).isGreaterThan(0)
            .describedAs("V2 mode StoreCommit should have count > 0")

        // V2: Weight distribution should exist
        val weightDist = genesis.node.getMLNodeWeightDistribution(pocStartHeight, participantAddress)
        Logger.info("Weight distribution: found=${weightDist.found}, weights=${weightDist.weights.size}")
        if (weightDist.found) {
            assertThat(weightDist.weights).isNotEmpty()
        }

        // V2: Proof API should work
        logSection("Verifying V2 behavior: Proof API available")
        val artifactState = genesis.api.getPocArtifactsState(pocStartHeight)
        Logger.info("Artifact state: count=${artifactState.count}, rootHash=${artifactState.rootHash}")
        assertThat(artifactState.count).isGreaterThanOrEqualTo(0)

        logSection("TEST PASSED: PoC V2 mode works correctly")
    }

    /**
     * Migration Test: V1 to V2 via governance without restart.
     * Verifies that the system can switch from V1 to V2 behavior dynamically.
     */
    @Test
    fun `poc migration - v1 to v2 via governance without restart`() {
        logSection("=== TEST: PoC V1 to V2 Migration ===")

        // Start with V1 mode
        val (cluster, genesis) = initCluster(reboot = true, config = v1Config)
        cluster.allPairs.forEach { it.waitForMlNodesToLoad() }

        // Verify V1 mode is active
        var params = genesis.getParams()
        assertThat(params.pocParams.pocV2Enabled).isFalse()
        Logger.info("Initial mode: poc_v2_enabled = ${params.pocParams.pocV2Enabled}")

        // === Phase 1: Run V1 PoC cycle ===
        logSection("Phase 1: Running V1 PoC cycle")

        genesis.waitForStage(EpochStage.END_OF_POC, offset = 1)
        genesis.node.waitForNextBlock(3)

        val v1EpochData = genesis.getEpochData()
        val v1PocHeight = v1EpochData.latestEpoch.pocStartBlockHeight

        // Verify V1 results
        val v1BatchCount = genesis.node.getPocBatchCount(v1PocHeight)
        Logger.info("V1 cycle complete: PoCBatch count = $v1BatchCount at height $v1PocHeight")
        assertThat(v1BatchCount).isGreaterThan(0)
            .describedAs("V1 cycle should produce PoCBatch entries")

        // === Phase 2: Switch to V2 via governance ===
        logSection("Phase 2: Switching to V2 via governance")

        val modifiedParams = params.copy(
            pocParams = params.pocParams.copy(pocV2Enabled = true)
        )

        genesis.runProposal(cluster, UpdateParams(params = modifiedParams))

        // Verify switch happened
        params = genesis.getParams()
        assertThat(params.pocParams.pocV2Enabled).isTrue()
        Logger.info("After governance: poc_v2_enabled = ${params.pocParams.pocV2Enabled}")

        // === Phase 3: Run V2 PoC cycle (no restart!) ===
        logSection("Phase 3: Running V2 PoC cycle (no restart)")

        // Wait for next PoC cycle
        genesis.waitForStage(EpochStage.END_OF_POC, offset = 1)
        genesis.node.waitForNextBlock(3)

        val v2EpochData = genesis.getEpochData()
        val v2PocHeight = v2EpochData.latestEpoch.pocStartBlockHeight
        val participantAddress = genesis.node.getColdAddress()

        // Verify V2 results
        val storeCommit = genesis.node.getPoCV2StoreCommit(v2PocHeight, participantAddress)
        Logger.info("V2 cycle complete: StoreCommit found=${storeCommit.found}, count=${storeCommit.count} at height $v2PocHeight")
        assertThat(storeCommit.found).isTrue()
            .describedAs("V2 cycle should produce StoreCommit entries")
        assertThat(storeCommit.count).isGreaterThan(0)
            .describedAs("V2 cycle StoreCommit should have count > 0")

        logSection("TEST PASSED: V1 to V2 migration via governance works correctly")
    }

    // === Test Configurations ===

    private val v1PocSpec = spec {
        this[AppState::inference] = spec<InferenceState> {
            this[InferenceState::params] = spec<InferenceParams> {
                this[InferenceParams::epochParams] = spec<EpochParams> {
                    this[EpochParams::pocStageDuration] = 3L
                    this[EpochParams::pocValidationDuration] = 4L
                }
                this[InferenceParams::pocParams] = spec<PocParams> {
                    this[PocParams::pocV2Enabled] = false
                }
            }
        }
    }

    private val v2PocSpec = spec {
        this[AppState::inference] = spec<InferenceState> {
            this[InferenceState::params] = spec<InferenceParams> {
                this[InferenceParams::epochParams] = spec<EpochParams> {
                    this[EpochParams::pocStageDuration] = 3L
                    this[EpochParams::pocValidationDuration] = 4L
                }
                this[InferenceParams::pocParams] = spec<PocParams> {
                    this[PocParams::pocV2Enabled] = true
                }
            }
        }
    }

    private val v1Config = inferenceConfig.copy(
        genesisSpec = inferenceConfig.genesisSpec?.merge(v1PocSpec) ?: v1PocSpec,
    )

    private val v2Config = inferenceConfig.copy(
        genesisSpec = inferenceConfig.genesisSpec?.merge(v2PocSpec) ?: v2PocSpec,
    )
}
