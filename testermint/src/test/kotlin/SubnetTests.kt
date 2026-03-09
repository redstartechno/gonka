import com.productscience.*
import org.assertj.core.api.Assertions.assertThat
import org.junit.jupiter.api.Test

class SubnetTests : TestermintTest() {

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
}
