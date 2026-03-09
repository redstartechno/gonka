package com.productscience.data

import com.google.gson.annotations.SerializedName

data class SubnetEscrowResponse(
    val escrow: SubnetEscrow?,
    val found: Boolean
)

data class SubnetEscrow(
    val id: String,
    val creator: String,
    val amount: String,
    val slots: List<String>,
    @SerializedName("epoch_index")
    val epochIndex: String,
    @SerializedName("app_hash")
    val appHash: String,
    val settled: Boolean
)
