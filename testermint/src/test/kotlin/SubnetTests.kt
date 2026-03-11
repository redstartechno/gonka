import com.productscience.*
import com.productscience.data.*
import org.assertj.core.api.Assertions.assertThat
import org.junit.jupiter.api.Test

class SubnetTests : TestermintTest() {

    private val noRestrictionsSpec = spec<AppState> {
        this[AppState::restrictions] = spec<RestrictionsState> {
            this[RestrictionsState::params] = spec<RestrictionsParams> {
                this[RestrictionsParams::restrictionEndBlock] = 0L
                this[RestrictionsParams::emergencyTransferExemptions] = emptyList<EmergencyTransferExemption>()
                this[RestrictionsParams::exemptionUsageTracking] = emptyList<ExemptionUsageEntry>()
            }
        }
    }

    private val noRestrictionsConfig = inferenceConfig.copy(
        genesisSpec = inferenceConfig.genesisSpec?.merge(noRestrictionsSpec) ?: noRestrictionsSpec
    )

    @Test
    fun `create subnet escrow and query it`() {
        val (cluster, genesis) = initCluster(reboot = true)

        // Wait for first epoch to complete so EffectiveEpochIndex is set.
        genesis.waitForNextEpoch()

        val creator = genesis.node.getColdAddress()
        val initialBalance = genesis.getBalance(creator)

        logSection("Creating subnet escrow")
        val escrowAmount = 7_000_000_000L  // 7 GNK
        val txResponse = genesis.createSubnetEscrow(escrowAmount)
        assertThat(txResponse.code).isEqualTo(0)

        logSection("Querying subnet escrow")
        val escrowResponse = genesis.node.querySubnetEscrow(1)
        assertThat(escrowResponse.found).isTrue()
        assertThat(escrowResponse.escrow).isNotNull()
        assertThat(escrowResponse.escrow!!.creator).isEqualTo(creator)
        assertThat(escrowResponse.escrow!!.amount).isEqualTo(escrowAmount.toString())
        assertThat(escrowResponse.escrow!!.slots).hasSize(16)  // SubnetGroupSize
        assertThat(escrowResponse.escrow!!.settled).isFalse()

        logSection("Verifying balance decreased")
        val balanceAfter = genesis.getBalance(creator)
        assertThat(balanceAfter).isEqualTo(initialBalance - escrowAmount)
    }

    @Test
    fun `subnet inference e2e with settlement`() {
        val (cluster, genesis) = initCluster(config = noRestrictionsConfig, reboot = true)
        genesis.waitForNextEpoch()

        cluster.allPairs.forEach { pair ->
            pair.mock?.setInferenceResponse(
                """{"id":"test","object":"chat.completion","created":0,"model":"$defaultModel","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}"""
            )
        }

        logSection("Creating separate user account")
        val userKeyName = "subnet-user"
        val userKey = genesis.node.createKey(userKeyName)
        val userAddress = userKey.address
        val fundAmount = 10_000_000_000L  // 10 GNK
        val transferResp = genesis.submitTransaction(
            listOf("bank", "send", genesis.node.getColdAddress(), userAddress, "${fundAmount}${genesis.config.denom}")
        )
        assertThat(transferResp.code).isEqualTo(0)
        val userBalance = genesis.getBalance(userAddress)
        assertThat(userBalance).isEqualTo(fundAmount)

        logSection("Creating subnet escrow from user account")
        val escrowAmount = 7_000_000_000L
        val txResp = genesis.createSubnetEscrow(escrowAmount, from = userKeyName)
        assertThat(txResp.code).isEqualTo(0)
        genesis.waitForNextInferenceWindow()

        logSection("Running subnetctl as user")
        val result = genesis.runSubnetctl(escrowId = 1, prompts = 20, model = defaultModel, keyName = userKeyName)

        logSection("Verifying settlement data")
        assertThat(result.parsed.escrowId).isEqualTo("1")
        assertThat(result.parsed.nonce).isGreaterThan(0)
        assertThat(result.parsed.hostStats).isNotEmpty()
        assertThat(result.parsed.signatures).isNotEmpty()
        val totalCompletedValidations = result.parsed.hostStats.sumOf { it.completedValidations }
        assertThat(totalCompletedValidations).isGreaterThan(0)

        logSection("Submitting settlement from user account")
        val settleResp = genesis.settleSubnetEscrow(result.rawJson, from = userKeyName)
        assertThat(settleResp.code).isEqualTo(0)

        logSection("Verifying escrow settled")
        val escrow = genesis.node.querySubnetEscrow(1)
        assertThat(escrow.escrow!!.settled).isTrue()

        logSection("Verifying user got refund")
        val balanceAfter = genesis.getBalance(userAddress)
        assertThat(balanceAfter).isGreaterThan(fundAmount - escrowAmount)
    }

    @Test
    fun `create escrow and query subnet mempool`() {
        val (cluster, genesis) = initCluster(reboot = true)

        // Wait for first epoch so EffectiveEpochIndex is set.
        genesis.waitForNextEpoch()

        logSection("Creating subnet escrow")
        val escrowAmount = 7_000_000_000L  // 7 GNK
        val txResponse = genesis.createSubnetEscrow(escrowAmount)
        assertThat(txResponse.code).isEqualTo(0)

        logSection("Query subnet mempool -- triggers lazy session creation")
        val mempool = genesis.api.getSubnetMempool(1)
        assertThat(mempool.txs).isNotNull()
        assertThat(mempool.txs).isEmpty()
    }
}
