package mlnode

import (
	"decentralized-api/broker"
	cosmos_client "decentralized-api/cosmosclient"
	"decentralized-api/internal/server/middleware"
	"decentralized-api/poc/artifacts"

	"github.com/labstack/echo/v4"
)

type Server struct {
	e             *echo.Echo
	recorder      cosmos_client.CosmosMessageClient
	broker        *broker.Broker
	artifactStore *artifacts.ManagedArtifactStore // Optional: for off-chain PoC artifacts
}

// ServerOption configures optional Server dependencies.
type ServerOption func(*Server)

// WithArtifactStore enables local artifact storage for off-chain PoC.
func WithArtifactStore(store *artifacts.ManagedArtifactStore) ServerOption {
	return func(s *Server) {
		s.artifactStore = store
	}
}

// TODO breacking changes: url path, support on mlnode side
func NewServer(recorder cosmos_client.CosmosMessageClient, broker *broker.Broker, opts ...ServerOption) *Server {
	e := echo.New()

	e.HTTPErrorHandler = middleware.TransparentErrorHandler

	e.Use(middleware.LoggingMiddleware)
	g := e.Group("/mlnode/v1/")

	s := &Server{
		e:        e,
		recorder: recorder,
		broker:   broker,
	}

	for _, opt := range opts {
		opt(s)
	}

	// keep old paths too for backward compatibility
	g.POST("poc-batches/generated", s.postGeneratedBatches)
	e.POST("/v1/poc-batches/generated", s.postGeneratedBatches)
	e.POST("/v2/poc-batches/generated", s.postGeneratedArtifactsV2)

	g.POST("poc-batches/validated", s.postValidatedBatches)
	e.POST("/v1/poc-batches/validated", s.postValidatedBatches)
	e.POST("/v2/poc-batches/validated", s.postValidatedArtifactsV2)
	return s
}

func (s *Server) Start(addr string) {
	go s.e.Start(addr)
}
