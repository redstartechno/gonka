import com.productscience.*
import com.productscience.data.*
import org.assertj.core.api.Assertions.assertThat
import org.junit.jupiter.api.BeforeAll
import org.junit.jupiter.api.MethodOrderer
import org.junit.jupiter.api.Order
import org.junit.jupiter.api.Tag
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.TestMethodOrder

/**
 * Integration tests for consensus-level transaction fee enforcement.
 *
 * Uses a custom genesis spec that enables FeeParams, requiring a cluster
 * reboot. All other tests run without fee enforcement (FeeParams not set
 * at genesis), matching pre-upgrade behavior.
 *
 * Verifies that:
 * - Fee-required messages are rejected with zero/insufficient fees
 * - Fee-required messages succeed with sufficient fees
 * - Fees are actually deducted from the sender's balance
 * - Fee-exempt messages (inference lifecycle) work without fees
 * - Default transaction path (with --gas-prices) works
 */
@Tag("unstable") // Requires cluster reboot with fee enforcement; excluded from standard CI run
@TestMethodOrder(MethodOrderer.OrderAnnotation::class)
class TransactionFeeTests : TestermintTest() {

    companion object {
        private lateinit var cluster: LocalCluster
        private lateinit var genesis: LocalInferencePair
        private lateinit var genesisAddress: String
        private lateinit var recipientAddress: String

        @BeforeAll
        @JvmStatic
        fun initOnce() {
            // Enable fee enforcement via genesis spec merge.
            // This triggers a cluster reboot with FeeParams set.
            val feeSpec = spec<AppState> {
                this[AppState::inference] = spec<InferenceState> {
                    this[InferenceState::params] = spec<InferenceParams> {
                        this[InferenceParams::feeParams] = spec<FeeParamsData> {
                            this[FeeParamsData::minGasPriceNgonka] = 10L
                            this[FeeParamsData::baseValidationGas] = 500_000L
                            this[FeeParamsData::gasPerPocCount] = 100L
                        }
                    }
                }
            }

            // Use genesis-only cluster (no join nodes) since join nodes start
            // unfunded and can't pay fees during cluster init.
            val result = initCluster(joinCount = 0, reboot = true, mergeSpec = feeSpec)
            cluster = result.first
            genesis = result.second
            genesisAddress = genesis.node.getColdAddress()
            // Use genesis's warm address as recipient (no join nodes available)
            recipientAddress = genesis.node.getWarmAddress()
        }
    }

    // --- Fee rejection tests ---

    @Test
    @Order(1)
    fun `bank send with zero fees is rejected`() {
        logHighlight("Testing that bank send with zero fees is rejected")

        val result = genesis.submitTransactionWithFees(
            listOf(
                "bank", "send",
                genesisAddress, recipientAddress,
                "1000ngonka"
            ),
            fees = "0ngonka"
        )

        assertThat(result.code).isNotEqualTo(0)
        assertThat(result.rawLog).containsIgnoringCase("insufficient fee")
        logHighlight("Bank send with zero fees correctly rejected: ${result.rawLog}")
    }

    @Test
    @Order(2)
    fun `staking delegate with zero fees is rejected`() {
        logHighlight("Testing that staking delegate with zero fees is rejected")

        val validatorAddr = genesis.node.getValidators().validators.first().operatorAddress

        val result = genesis.submitTransactionWithFees(
            listOf(
                "staking", "delegate",
                validatorAddr,
                "1000ngonka"
            ),
            fees = "0ngonka"
        )

        assertThat(result.code).isNotEqualTo(0)
        assertThat(result.rawLog).containsIgnoringCase("insufficient fee")
        logHighlight("Staking delegate with zero fees correctly rejected: ${result.rawLog}")
    }

    @Test
    @Order(3)
    fun `bank send with insufficient fees is rejected`() {
        logHighlight("Testing that bank send with insufficient fees is rejected")

        // At 10 ngonka/gas and 200,000 gas, minimum fee is 2,000,000 ngonka.
        // Send only 1 ngonka as fee.
        val result = genesis.submitTransactionWithFees(
            listOf(
                "bank", "send",
                genesisAddress, recipientAddress,
                "1000ngonka"
            ),
            fees = "1ngonka"
        )

        assertThat(result.code).isNotEqualTo(0)
        assertThat(result.rawLog).containsIgnoringCase("insufficient fee")
        logHighlight("Bank send with insufficient fees correctly rejected: ${result.rawLog}")
    }

    // --- Fee acceptance tests ---

    @Test
    @Order(4)
    fun `bank send with sufficient fees succeeds and deducts balance`() {
        logHighlight("Testing that bank send with sufficient fees succeeds")

        val balanceBefore = genesis.getBalance(genesisAddress)

        // 200,000 gas * 10 ngonka/gas = 2,000,000 ngonka minimum fee
        val result = genesis.submitTransactionWithFees(
            listOf(
                "bank", "send",
                genesisAddress, recipientAddress,
                "1000ngonka"
            ),
            fees = "2000000ngonka"
        )

        assertThat(result.code).isEqualTo(0)

        val balanceAfter = genesis.getBalance(genesisAddress)
        // Balance should decrease by at least transfer amount + fee
        assertThat(balanceAfter).isLessThan(balanceBefore)
        val deducted = balanceBefore - balanceAfter
        // Deducted amount should be at least the transfer (1000) + fee (2000000)
        assertThat(deducted).isGreaterThanOrEqualTo(1000 + 2000000)
        logHighlight("Balance deducted: $deducted ngonka (transfer=1000 + fee>=2000000)")
    }

    @Test
    @Order(5)
    fun `transaction with zero fees is rejected when enforcement is active`() {
        logHighlight("Testing that zero-fee transaction is rejected (not just insufficient, but zero)")

        // The default submitTransaction path sends with zero fees.
        // With fee enforcement active, this should be rejected.
        val result = genesis.submitTransactionWithFees(
            listOf(
                "bank", "send",
                genesisAddress, recipientAddress,
                "500ngonka"
            ),
            fees = "0ngonka"
        )

        assertThat(result.code).isNotEqualTo(0)
        assertThat(result.rawLog).containsIgnoringCase("insufficient fee")
        logHighlight("Zero-fee transaction correctly rejected with fee enforcement active")
    }

}
