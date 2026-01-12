package mlnode

import (
	cosmos_client "decentralized-api/cosmosclient"
	"decentralized-api/logging"
	"decentralized-api/mlnodeclient"
	"encoding/base64"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/productscience/inference/api/inference/inference"
	"github.com/productscience/inference/x/inference/types"
)

// postGeneratedArtifactsV2 handles PoC v2 artifact batch callbacks from MLNode.
// Receives artifact batches and submits them to chain via MsgSubmitPocArtifactBatchesV2.
// Note: Current iteration stores on-chain; later iteration moves fully off-chain.
func (s *Server) postGeneratedArtifactsV2(ctx echo.Context) error {
	var body mlnodeclient.GeneratedArtifactBatchV2

	if err := ctx.Bind(&body); err != nil {
		logging.Error("ArtifactBatchV2-callback. Failed to decode request body", types.PoC, "error", err)
		return echo.NewHTTPError(http.StatusBadRequest, err)
	}

	logging.Debug("ArtifactBatchV2-callback. Received", types.PoC,
		"blockHeight", body.BlockHeight,
		"publicKey", body.PublicKey,
		"nodeId", body.NodeId,
		"artifactsCount", len(body.Artifacts))

	// Look up node_id string from node number
	var nodeId string
	node, found := s.broker.GetNodeByNodeNum(uint64(body.NodeId))
	if found {
		nodeId = node.Id
		logging.Info("ArtifactBatchV2-callback. Found node by node num", types.PoC,
			"nodeId", nodeId,
			"nodeNum", body.NodeId)
	} else {
		logging.Warn("ArtifactBatchV2-callback. Unknown NodeNum. Sending with empty nodeId",
			types.PoC, "node_num", body.NodeId)
	}

	// Convert artifacts from JSON format to proto format
	protoArtifacts := make([]*inference.PoCArtifactV2, 0, len(body.Artifacts))
	for _, a := range body.Artifacts {
		// Decode base64 vector to bytes
		vectorBytes, err := base64.StdEncoding.DecodeString(a.VectorB64)
		if err != nil {
			logging.Warn("ArtifactBatchV2-callback. Failed to decode artifact vector", types.PoC,
				"nonce", a.Nonce, "error", err)
			continue
		}
		protoArtifacts = append(protoArtifacts, &inference.PoCArtifactV2{
			Nonce:  a.Nonce,
			Vector: vectorBytes,
		})
	}

	// Use batch submission (wrapping single batch from this callback)
	msg := &inference.MsgSubmitPocArtifactBatchesV2{
		Batches: []*inference.PoCArtifactBatchV2{
			{
				PocStageStartBlockHeight: body.BlockHeight,
				NodeId:                   nodeId,
				Artifacts:                protoArtifacts,
			},
		},
	}

	if err := s.recorder.SubmitPocArtifactBatchesV2(msg); err != nil {
		logging.Error("ArtifactBatchV2-callback. Failed to submit MsgSubmitPocArtifactBatchesV2", types.PoC, "error", err)
		return err
	}

	logging.Info("ArtifactBatchV2-callback. Submitted artifact batch", types.PoC,
		"blockHeight", body.BlockHeight,
		"nodeId", nodeId,
		"artifactsCount", len(protoArtifacts))

	return ctx.NoContent(http.StatusOK)
}

// postValidatedArtifactsV2 handles PoC v2 validation result callbacks from MLNode.
// Receives validation results and submits them to chain via MsgSubmitPocValidationsV2 (batch).
func (s *Server) postValidatedArtifactsV2(ctx echo.Context) error {
	var body mlnodeclient.ValidatedResultV2

	if err := ctx.Bind(&body); err != nil {
		logging.Error("ValidatedArtifactsV2-callback. Failed to decode request body", types.PoC, "error", err)
		return echo.NewHTTPError(http.StatusBadRequest, err)
	}

	logging.Debug("ValidatedArtifactsV2-callback. Received", types.PoC,
		"blockHeight", body.BlockHeight,
		"publicKey", body.PublicKey,
		"nTotal", body.NTotal,
		"fraudDetected", body.FraudDetected)

	// Convert public key to bech32 address
	address, err := cosmos_client.PubKeyToAddress(body.PublicKey)
	if err != nil {
		logging.Error("ValidatedArtifactsV2-callback. Failed to convert public key to address", types.PoC,
			"publicKey", body.PublicKey,
			"nTotal", body.NTotal,
			"fraudDetected", body.FraudDetected,
			"error", err)
		return err
	}

	// Convert fraud_detected + n_total to validated_weight
	validatedWeight := body.ToValidatedWeight()

	logging.Info("ValidatedArtifactsV2-callback. Submitting validation", types.PoC,
		"participant", address,
		"validatedWeight", validatedWeight,
		"fraudDetected", body.FraudDetected)

	// Use batch submission (even for single validation - no single-validation RPC exists)
	msg := &inference.MsgSubmitPocValidationsV2{
		Validations: []*inference.PoCValidationV2{
			{
				ParticipantAddress:       address,
				PocStageStartBlockHeight: body.BlockHeight,
				ValidatedWeight:          validatedWeight,
			},
		},
	}

	if err := s.recorder.SubmitPocValidationsV2(msg); err != nil {
		logging.Error("ValidatedArtifactsV2-callback. Failed to submit MsgSubmitPocValidationsV2", types.PoC,
			"participant", address,
			"error", err)
		return err
	}

	return ctx.NoContent(http.StatusOK)
}
