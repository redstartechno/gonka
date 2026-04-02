import com.productscience.*
import com.productscience.data.*
import org.assertj.core.api.Assertions.assertThat
import org.junit.jupiter.api.BeforeAll
import org.junit.jupiter.api.MethodOrderer
import org.junit.jupiter.api.Order
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.TestMethodOrder

/**
 * Integration tests verifying transaction fee infrastructure.
 *
 * These tests run on a standard cluster WITHOUT fee enforcement (FeeParams
 * not set at genesis). They verify that the fee infrastructure code doesn't
 * break existing functionality.
 *
 * Consensus-level fee enforcement logic (rejection of zero-fee txs, bypass
 * for duty messages, count-linear PoC fees) is covered by unit tests in
 * ante_fee_test.go.
 */
@TestMethodOrder(MethodOrderer.OrderAnnotation::class)
class TransactionFeeTests : TestermintTest() {

    companion object {
        private lateinit var cluster: LocalCluster
        private lateinit var genesis: LocalInferencePair

        @BeforeAll
        @JvmStatic
        fun initOnce() {
            val result = initCluster()
            cluster = result.first
            genesis = result.second
        }
    }

    @Test
    @Order(1)
    fun `fee params are nil at genesis`() {
        logHighlight("Verifying FeeParams are not set at genesis")

        val params = genesis.getParams()
        // FeeParams should be null/absent at genesis (enabled via v0.2.12 upgrade handler)
        assertThat(params.feeParams).isNull()
        logHighlight("FeeParams correctly nil at genesis")
    }

    @Test
    @Order(2)
    fun `inference succeeds with fee infrastructure in place`() {
        logHighlight("Testing that inference pipeline works with fee infrastructure code present")

        genesis.waitForNextInferenceWindow()
        val response = genesis.makeInferenceRequest(inferenceRequest)

        // The DAPI submits MsgStartInference and MsgFinishInference which
        // pass through the fee bypass decorator (no-op when FeeParams nil).
        assertThat(response.choices).isNotEmpty
        logHighlight("Inference succeeded: model=${response.model}")
    }
}
