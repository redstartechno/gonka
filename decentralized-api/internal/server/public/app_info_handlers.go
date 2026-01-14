package public

import (
	"decentralized-api/logging"
	"net/http"
	"syscall"
	"time"

	"github.com/cosmos/cosmos-sdk/client/grpc/cmtservice"
	"github.com/cosmos/cosmos-sdk/version"
	"github.com/labstack/echo/v4"
	"github.com/productscience/inference/x/inference/types"
)

const pocApiVersion = "2.0.1"

func (s *Server) getVersions(ctx echo.Context) error {
	cometClient := s.recorder.NewCometQueryClient()
	resp, err := cometClient.GetNodeInfo(s.recorder.GetContext(), &cmtservice.GetNodeInfoRequest{})
	if err != nil {
		logging.Error("Failed to get node info from cosmos node", types.Server, "error", err)
		return ctx.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to get node info",
		})
	}
	nodeVersion := resp.ApplicationVersion

	var rLimit syscall.Rlimit
	_ = syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit)

	return ctx.JSON(http.StatusOK, map[string]any{
		"timestamp":       time.Now().UTC().Format(time.RFC3339),
		"poc_api_version": pocApiVersion,
		"fd_limit":        rLimit.Cur,
		"api_version": map[string]string{
			"application_name": version.AppName,
			"version":          version.Version,
			"commit":           version.Commit,
		},
		"node_version": map[string]string{
			"application_name": nodeVersion.Name,
			"version":          nodeVersion.Version,
			"commit":           nodeVersion.GitCommit,
		},
	})
}
