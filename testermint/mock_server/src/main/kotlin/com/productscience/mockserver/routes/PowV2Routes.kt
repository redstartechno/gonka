package com.productscience.mockserver.routes

import com.productscience.mockserver.getHost
import com.productscience.mockserver.model.*
import com.productscience.mockserver.service.HostName
import io.ktor.server.application.*
import io.ktor.server.response.*
import io.ktor.server.routing.*
import io.ktor.server.request.*
import io.ktor.http.*
import com.productscience.mockserver.service.WebhookService
import org.slf4j.LoggerFactory

/**
 * Configures routes for PoC v2 (artifact-based) endpoints.
 * These are the /api/v1/inference/pow/* endpoints that proxy through MLNode to vLLM.
 */
fun Route.powV2Routes(webhookService: WebhookService) {
    val logger = LoggerFactory.getLogger("PowV2Routes")

    // POST /api/v1/inference/pow/init/generate - Fan-out generate to all backends
    post("/api/v1/inference/pow/init/generate") {
        handleInitGenerateV2(call, webhookService, logger)
    }

    // Versioned POST /{version}/api/v1/inference/pow/init/generate
    post("/{version}/api/v1/inference/pow/init/generate") {
        val version = call.parameters["version"]
        logger.info("Received versioned PoC v2 init/generate request for version: $version")
        handleInitGenerateV2(call, webhookService, logger)
    }

    // POST /api/v1/inference/pow/generate - Generate/validate specific nonces
    post("/api/v1/inference/pow/generate") {
        handleGenerateV2(call, webhookService, logger)
    }

    // Versioned POST /{version}/api/v1/inference/pow/generate
    post("/{version}/api/v1/inference/pow/generate") {
        val version = call.parameters["version"]
        logger.info("Received versioned PoC v2 generate request for version: $version")
        handleGenerateV2(call, webhookService, logger)
    }

    // GET /api/v1/inference/pow/status - Aggregate status from all backends
    get("/api/v1/inference/pow/status") {
        handlePowStatusV2(call, logger)
    }

    // Versioned GET /{version}/api/v1/inference/pow/status
    get("/{version}/api/v1/inference/pow/status") {
        val version = call.parameters["version"]
        logger.debug("Received versioned PoC v2 status request for version: $version")
        handlePowStatusV2(call, logger)
    }

    // POST /api/v1/inference/pow/stop - Fan-out stop to all backends
    post("/api/v1/inference/pow/stop") {
        handlePowStopV2(call, logger)
    }

    // Versioned POST /{version}/api/v1/inference/pow/stop
    post("/{version}/api/v1/inference/pow/stop") {
        val version = call.parameters["version"]
        logger.info("Received versioned PoC v2 stop request for version: $version")
        handlePowStopV2(call, logger)
    }
}

/**
 * Handles PoC v2 init/generate requests.
 * Triggers webhook callback to /generated with artifact batches.
 */
private suspend fun handleInitGenerateV2(call: ApplicationCall, webhookService: WebhookService, logger: org.slf4j.Logger) {
    logger.info("Received PoC v2 init/generate request")
    
    val host = call.getHost()
    if (getModelState(host) != ModelState.STOPPED ||
        getPowState(host) != PowState.POW_STOPPED) {
        logger.warn("Invalid state for PoC v2 init/generate. Current state: ${getModelState(host)}, POW state: ${getPowState(host)}")
        call.respond(HttpStatusCode.BadRequest, mapOf(
            "error" to "Invalid state for generation. state = ${getModelState(host)}. powState = ${getPowState(host)}"
        ))
        return
    }

    setModelState(host, ModelState.POW)
    setPowState(host, PowState.POW_GENERATING)
    logger.info("State updated to POW with POW_GENERATING substate (v2)")

    val requestBody = call.receiveText()
    logger.debug("Processing PoC v2 generate webhook with request body: $requestBody")

    // Process the webhook asynchronously - sends artifacts to callback URL
    webhookService.processGeneratePocV2Webhook(requestBody, HostName(call.getHost()))

    call.respond(HttpStatusCode.OK, mapOf(
        "status" to "OK",
        "backends" to 1,
        "n_groups" to 1
    ))
}

/**
 * Handles PoC v2 /generate requests (validation flow).
 * Triggers webhook callback to /validated with validation results.
 */
private suspend fun handleGenerateV2(call: ApplicationCall, webhookService: WebhookService, logger: org.slf4j.Logger) {
    logger.info("Received PoC v2 generate (validation) request")

    val host = call.getHost()
    // This can be called during POW_GENERATING or POW_VALIDATING states
    if (getModelState(host) != ModelState.POW) {
        logger.warn("Invalid state for PoC v2 generate. Current state: ${getModelState(host)}, POW state: ${getPowState(host)}")
        call.respond(HttpStatusCode.BadRequest, mapOf("error" to "Invalid state for validation"))
        return
    }

    // Update state to validating
    setPowState(host, PowState.POW_VALIDATING)

    val requestBody = call.receiveText()
    logger.debug("Processing PoC v2 validation webhook with request body: $requestBody")

    // Process the webhook asynchronously - sends validation result to callback URL
    webhookService.processValidatePocV2Webhook(requestBody)

    call.respond(HttpStatusCode.OK, mapOf(
        "status" to "completed",
        "request_id" to "mock-validation-request-id"
    ))
}

/**
 * Handles PoC v2 status requests.
 */
private suspend fun handlePowStatusV2(call: ApplicationCall, logger: org.slf4j.Logger) {
    logger.debug("Received PoC v2 status request")
    val powState = getPowState(call.getHost())
    val statusStr = when (powState) {
        PowState.POW_GENERATING -> "GENERATING"
        PowState.POW_VALIDATING -> "IDLE"
        else -> "IDLE"
    }
    call.respond(
        HttpStatusCode.OK,
        mapOf(
            "status" to statusStr,
            "backends" to listOf(
                mapOf("port" to 5001, "status" to statusStr)
            )
        )
    )
}

/**
 * Handles PoC v2 stop requests.
 */
private suspend fun handlePowStopV2(call: ApplicationCall, logger: org.slf4j.Logger) {
    logger.info("Received PoC v2 stop request")
    
    val host = call.getHost()
    setModelState(host, ModelState.STOPPED)
    setPowState(host, PowState.POW_STOPPED)

    call.respond(HttpStatusCode.OK, mapOf(
        "status" to "OK",
        "results" to listOf(mapOf("port" to 5001, "status" to "stopped")),
        "errors" to null
    ))
}
