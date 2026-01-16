import com.productscience.*
import com.productscience.data.*
import org.assertj.core.api.Assertions.assertThat
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.Timeout
import org.tinylog.kotlin.Logger
import java.nio.ByteBuffer
import java.nio.ByteOrder
import java.security.MessageDigest
import java.time.Instant
import java.util.*
import java.util.concurrent.TimeUnit

@Timeout(value = 15, unit = TimeUnit.MINUTES)
class PoCOffChainTests : TestermintTest() {

    @Test
    fun `poc proofs endpoint returns valid proofs after poc cycle`() {
        logSection("=== TEST: PoC Off-Chain Proofs API ===")

        // Initialize cluster with default configuration
        val (cluster, genesis) = initCluster(reboot = true)
        cluster.allPairs.forEach { it.waitForMlNodesToLoad() }
        // Wait for PoC generation phase to end
        genesis.waitForStage(EpochStage.END_OF_POC, offset = 1)

        logSection("Querying artifact store state")

        // Get current epoch info
        val epochData = genesis.getEpochData()
        val pocStartHeight = epochData.latestEpoch.pocStartBlockHeight

        // Query actual artifact store state
        val artifactState = genesis.api.getPocArtifactsState(pocStartHeight)
        Logger.info("Artifact store state: count=${artifactState.count}, rootHash=${artifactState.rootHash}")

        // Skip if no artifacts stored
        if (artifactState.count == 0L) {
            Logger.warn("No artifacts stored for epoch $pocStartHeight, skipping proof test")
            return
        }

        logSection("Building proof request with real values")

        val validatorAddress = genesis.node.getColdAddress()
        val validatorSignerAddress = genesis.node.getColdAddress()
        val timestamp = Instant.now().toEpochNanos()

        // Use actual values from artifact store
        val rootHash = artifactState.rootHash
        val count = artifactState.count
        // Request first few artifacts (up to 3, or less if fewer exist)
        val leafIndices = (0 until minOf(3, count.toInt())).map { it.toLong() }

        // Build and sign the request payload
        val signPayload = buildPocProofsSignPayload(
            pocStartHeight,
            Base64.getDecoder().decode(rootHash),
            count,
            leafIndices,
            timestamp,
            validatorAddress,
            validatorSignerAddress
        )
        val signPayloadHex = signPayload.joinToString("") { "%02x".format(it) }

        // Sign the payload using the node's key
        val signature = genesis.node.signPayload(signPayloadHex)

        val request = PocProofsRequest(
            pocStageStartBlockHeight = pocStartHeight,
            rootHash = rootHash,
            count = count,
            leafIndices = leafIndices,
            validatorAddress = validatorAddress,
            validatorSignerAddress = validatorSignerAddress,
            timestamp = timestamp,
            signature = signature
        )

        logSection("Requesting proofs")

        // Make the request
        val response = genesis.api.getPocProofsRaw(request)
        val statusCode = response.second.statusCode

        Logger.info("PoC proofs response status: $statusCode")
        Logger.info("Response body: ${response.third}")

        // Should succeed with real values
        assertThat(statusCode).isEqualTo(200)

        val proofResponse = cosmosJson.fromJson(response.third.get(), PocProofsResponse::class.java)
        assertThat(proofResponse.proofs).hasSize(leafIndices.size)

        proofResponse.proofs.forEach { proof ->
            assertThat(proof.leafIndex).isIn(*leafIndices.toTypedArray())
            assertThat(proof.vectorBytes).isNotEmpty()
            assertThat(proof.proof).isNotEmpty()
            Logger.info("Received proof for leaf ${proof.leafIndex}: nonce=${proof.nonceValue}, proofLen=${proof.proof.size}")
        }

        logSection("TEST PASSED: PoC proofs endpoint returns valid proofs")
    }

    companion object {
        /**
         * Builds the binary payload for PoC proofs signature verification.
         * Format: SHA256(poc_stage_start_block_height(LE64) || root_hash(32) || count(LE32) ||
         *         leaf_indices(LE32 each) || timestamp(LE64) || validator_address || validator_signer_address)
         */
        fun buildPocProofsSignPayload(
            pocStageStartBlockHeight: Long,
            rootHash: ByteArray,
            count: Long,
            leafIndices: List<Long>,
            timestamp: Long,
            validatorAddress: String,
            validatorSignerAddress: String
        ): ByteArray {
            // Calculate buffer size
            val size = 8 + 32 + 4 + (leafIndices.size * 4) + 8 +
                    validatorAddress.toByteArray().size + validatorSignerAddress.toByteArray().size

            val buffer = ByteBuffer.allocate(size)
            buffer.order(ByteOrder.LITTLE_ENDIAN)

            buffer.putLong(pocStageStartBlockHeight)
            buffer.put(rootHash)
            buffer.putInt(count.toInt())
            leafIndices.forEach { buffer.putInt(it.toInt()) }
            buffer.putLong(timestamp)
            buffer.put(validatorAddress.toByteArray())
            buffer.put(validatorSignerAddress.toByteArray())

            // SHA256 hash
            val digest = MessageDigest.getInstance("SHA-256")
            return digest.digest(buffer.array())
        }
    }
}
