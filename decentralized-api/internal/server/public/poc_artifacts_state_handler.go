package public

import (
	"encoding/base64"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"
)

// PocArtifactsStateResponse is the response for GET /v1/poc/artifacts/state
type PocArtifactsStateResponse struct {
	PocStageStartBlockHeight int64  `json:"poc_stage_start_block_height"`
	Count                    uint32 `json:"count"`
	RootHash                 string `json:"root_hash"` // base64-encoded 32 bytes, empty if count=0
}

// getPocArtifactsState returns the current artifact store state for a given height.
// Used by validators/testermint to get real count and root_hash for proof requests.
func (s *Server) getPocArtifactsState(ctx echo.Context) error {
	if s.artifactStore == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "artifact store not configured")
	}

	heightParam := ctx.QueryParam("height")
	if heightParam == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "height query parameter required")
	}

	height, err := strconv.ParseInt(heightParam, 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid height parameter")
	}

	store, err := s.artifactStore.GetStore(height)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "epoch not found (may be pruned or not yet created)")
	}

	count := store.Count()
	rootHash := store.GetRoot()

	var rootHashB64 string
	if rootHash != nil {
		rootHashB64 = base64.StdEncoding.EncodeToString(rootHash)
	}

	return ctx.JSON(http.StatusOK, PocArtifactsStateResponse{
		PocStageStartBlockHeight: height,
		Count:                    count,
		RootHash:                 rootHashB64,
	})
}
