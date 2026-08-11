// Copyright 2026 AxonFlow
// SPDX-License-Identifier: BUSL-1.1
//
// Entrypoint for the ExtMcp shim (AID-212 follow-up): serves the Envoy ext_proc
// v3 processor that governs MCP tool args/results via check-input/check-output.
// Same env config as the ext_authz adapter; default listen :9091 to run
// alongside it.
package main

import (
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	ext_proc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	adapter "github.com/getaxonflow/axonflow/examples/integrations/agentgateway-adapter"
)

func main() {
	cfg := adapter.LoadConfigFromEnv()
	if os.Getenv("MCP_SHIM_LISTEN") != "" {
		cfg.Listen = os.Getenv("MCP_SHIM_LISTEN")
	} else if cfg.Listen == ":9090" {
		cfg.Listen = ":9091" // default: alongside the ext_authz adapter
	}
	mcp := adapter.NewMCPClient(cfg)
	proc := adapter.NewExtMcpServer(cfg, mcp)

	lis, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Fatalf("listen %s: %v", cfg.Listen, err)
	}

	srv := grpc.NewServer()
	ext_proc.RegisterExternalProcessorServer(srv, proc)

	hs := health.NewServer()
	healthpb.RegisterHealthServer(srv, hs)
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	reflection.Register(srv)

	log.Printf("axonflow-mcp-shim: ext_proc v3 listening on %s -> PDP %s (check-input/check-output, fail_mode=%s)",
		cfg.Listen, cfg.AxonFlowEndpoint, cfg.FailMode)

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		<-c
		srv.GracefulStop()
	}()

	if err := srv.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
