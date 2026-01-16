package mlnode

import (
	"decentralized-api/logging"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/productscience/inference/x/inference/types"
)

const deprecationMessage = "PoC v1 callbacks are deprecated; use /v2/poc-batches/generated"

// postGeneratedBatches is a deprecated V1 handler that returns 410 Gone.
// All PoC batch submissions should use the V2 endpoint.
func (s *Server) postGeneratedBatches(ctx echo.Context) error {
	logging.Warn("postGeneratedBatches: V1 PoC callback is deprecated", types.PoC,
		"path", ctx.Path(),
		"remote_addr", ctx.RealIP())
	return echo.NewHTTPError(http.StatusGone, deprecationMessage)
}

// postValidatedBatches is a deprecated V1 handler that returns 410 Gone.
// All PoC validation submissions should use the V2 endpoint.
func (s *Server) postValidatedBatches(ctx echo.Context) error {
	logging.Warn("postValidatedBatches: V1 PoC callback is deprecated", types.PoC,
		"path", ctx.Path(),
		"remote_addr", ctx.RealIP())
	return echo.NewHTTPError(http.StatusGone, deprecationMessage)
}
