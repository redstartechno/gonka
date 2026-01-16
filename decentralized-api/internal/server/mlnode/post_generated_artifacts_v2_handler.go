package mlnode

import (
	cosmos_client "decentralized-api/cosmosclient"
	"decentralized-api/logging"
	"decentralized-api/mlnodeclient"
	"decentralized-api/pocartifacts"
	"encoding/base64"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/productscience/inference/api/inference/inference"
	"github.com/productscience/inference/x/inference/types"
)

// postGeneratedArtifactsV2 handles PoC v2 artifact batch callbacks from MLNode.
// Receives artifact batches and submits them to chain via MsgSubmitPocBatchesV2.
// If artifactStore is configured, also stores artifacts locally for off-chain proofs.
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

	// Look up node_id string from node number (chain requires non-empty nodeId)
	node, found := s.broker.GetNodeByNodeNum(uint64(body.NodeId))
	if !found {
		logging.Error("ArtifactBatchV2-callback. Unknown NodeNum", types.PoC, "node_num", body.NodeId)
		return echo.NewHTTPError(http.StatusBadRequest, "unknown node_num")
	}
	nodeId := node.Id
	logging.Info("ArtifactBatchV2-callback. Found node by node num", types.PoC,
		"nodeId", nodeId,
		"nodeNum", body.NodeId)

	// Convert artifacts from JSON format to proto format
	protoArtifacts := make([]*inference.PoCArtifactV2, 0, len(body.Artifacts))
	for _, a := range body.Artifacts {
		vectorBytes, err := base64.StdEncoding.DecodeString(a.VectorB64)
		if err != nil {
			logging.Error("ArtifactBatchV2-callback. Failed to decode artifact vector", types.PoC,
				"nonce", a.Nonce, "error", err)
			return echo.NewHTTPError(http.StatusBadRequest, "invalid base64 in artifact vector")
		}
		if len(vectorBytes) == 0 {
			logging.Error("ArtifactBatchV2-callback. Empty artifact vector", types.PoC,
				"nonce", a.Nonce)
			return echo.NewHTTPError(http.StatusBadRequest, "empty artifact vector")
		}
		protoArtifacts = append(protoArtifacts, &inference.PoCArtifactV2{
			Nonce:  a.Nonce,
			Vector: vectorBytes,
		})
	}

	// Store artifacts locally if artifact store is configured (dual-write mode)
	if s.artifactStore != nil {
		epochStore, err := s.artifactStore.GetOrCreateStore(body.BlockHeight)
		if err != nil {
			logging.Error("ArtifactBatchV2-callback. Failed to get epoch store", types.PoC,
				"blockHeight", body.BlockHeight, "error", err)
		} else {
			storedCount := 0
			for _, a := range body.Artifacts {
				vectorBytes, _ := base64.StdEncoding.DecodeString(a.VectorB64)
				err := epochStore.Add(int32(a.Nonce), vectorBytes)
				if err != nil {
					if errors.Is(err, pocartifacts.ErrDuplicateNonce) {
						logging.Debug("ArtifactBatchV2-callback. Duplicate nonce skipped", types.PoC,
							"nonce", a.Nonce)
						continue
					}
					logging.Error("ArtifactBatchV2-callback. Failed to store artifact locally", types.PoC,
						"nonce", a.Nonce, "error", err)
				} else {
					storedCount++
				}
			}
			logging.Debug("ArtifactBatchV2-callback. Stored artifacts locally", types.PoC,
				"blockHeight", body.BlockHeight,
				"storedCount", storedCount,
				"totalCount", epochStore.Count())
		}
	}

	// Use batch submission (wrapping single batch from this callback)
	msg := &inference.MsgSubmitPocBatchesV2{
		PocStageStartBlockHeight: body.BlockHeight,
		Batches: []*inference.PoCBatchPayloadV2{
			{
				NodeId:    nodeId,
				Artifacts: protoArtifacts,
			},
		},
	}

	if err := s.recorder.SubmitPocBatchesV2(msg); err != nil {
		logging.Error("BatchV2-callback. Failed to submit MsgSubmitPocBatchesV2", types.PoC, "error", err)
		return err
	}

	logging.Info("BatchV2-callback. Submitted batch", types.PoC,
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
	// PoC validation provides hex-encoded public keys
	address, err := cosmos_client.PubKeyHexToAddress(body.PublicKey)
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
		PocStageStartBlockHeight: body.BlockHeight,
		Validations: []*inference.PoCValidationPayloadV2{
			{
				ParticipantAddress: address,
				ValidatedWeight:    validatedWeight,
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
