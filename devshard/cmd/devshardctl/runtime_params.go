package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"strings"

	"common/chain"
	mlnodeclient "common/nodemanager"
	"devshard/bridge"
	"devshard/runtimeconfig"
	"devshard/runtimeparams"
)

func initGatewayRuntimeParams(ctx context.Context, chainGRPC string) (*runtimeparams.Managed, func(), error) {
	env := runtimeparams.SettingsFromEnv()

	var nmClient *mlnodeclient.Client
	var nmClose func()
	if env.NodeManagerAddr != "" {
		client, err := mlnodeclient.NewClient(env.NodeManagerAddr)
		if err != nil {
			slog.Warn("runtime params: nodemanager dial failed; chain fallback only",
				"addr", env.NodeManagerAddr, "err", err)
		} else {
			nmClient = client
			nmClose = func() { _ = client.Close() }
		}
	}

	chainFetcher, chainClose, err := newGatewayChainFetcher(chainGRPC)
	if err != nil {
		if nmClose != nil {
			nmClose()
		}
		return nil, nil, err
	}

	setup := runtimeparams.SetupConfig{
		Chain:  chainFetcher,
		Logger: slog.Default(),
		Env:    env,
	}
	if nmClient != nil {
		setup.GRPCClient = nmClient.NodeManagerClient()
	}

	managed, err := runtimeparams.NewManaged(ctx, setup)
	if err != nil {
		if chainClose != nil {
			chainClose()
		}
		if nmClose != nil {
			nmClose()
		}
		return nil, nil, err
	}

	closeAll := func() {
		managed.Close()
		if chainClose != nil {
			chainClose()
		}
		if nmClose != nil {
			nmClose()
		}
	}
	return managed, closeAll, nil
}

func newGatewayChainFetcher(chainGRPC string) (runtimeconfig.ChainParamsFetcher, func(), error) {
	chainGRPC = strings.TrimSpace(chainGRPC)
	if chainGRPC == "" {
		return nil, nil, fmt.Errorf("chain gRPC URL is required")
	}
	client, err := chain.New(chainGRPC)
	if err != nil {
		return nil, nil, fmt.Errorf("chain gRPC dial %s: %w", chainGRPC, err)
	}
	closeFn := func() {
		if c, ok := client.Conn().(interface{ Close() error }); ok {
			_ = c.Close()
		}
	}
	slog.Info("runtime params chain fetcher", "transport", "grpc", "url", chainGRPC)
	return runtimeparams.NewGRPCChainFetcher(client), closeFn, nil
}

func mustInitGatewayRuntimeParams(ctx context.Context, chainGRPC string) (*runtimeparams.Managed, func()) {
	managed, closeFn, err := initGatewayRuntimeParams(ctx, chainGRPC)
	if err != nil {
		log.Fatalf("runtime params provider: %v", err)
	}
	if managed == nil {
		log.Fatal("runtime params provider: nil setup")
	}
	return managed, closeFn
}

type runtimeBuildDeps struct {
	bridge       bridge.MainnetBridge
	chainClient  *chain.Client
	defaultModel string
	perf         *PerfTracker
}

func (d runtimeBuildDeps) validate() error {
	if d.bridge == nil || d.chainClient == nil {
		return fmt.Errorf("chain gRPC client is required")
	}
	return nil
}
