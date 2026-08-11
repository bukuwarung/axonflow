// Copyright 2026 AxonFlow
// SPDX-License-Identifier: BUSL-1.1
//
// Entrypoint for the agentgateway ext_authz adapter (AID-100). Wires the
// env config -> PDP client -> ext_authz v3 server and serves gRPC (plus a
// health service for agentgateway / docker-compose probes). The adapter logic
// lives in the parent `adapter` package; this file only starts it.
package main

import (
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	adapter "github.com/getaxonflow/axonflow/examples/integrations/agentgateway-adapter"
)

func main() {
	cfg := adapter.LoadConfigFromEnv()
	pdp := adapter.NewPDPClient(cfg)
	authz := adapter.NewAuthzServer(cfg, pdp)

	lis, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Fatalf("listen %s: %v", cfg.Listen, err)
	}

	srv := grpc.NewServer()
	authv3.RegisterAuthorizationServer(srv, authz)

	hs := health.NewServer()
	healthpb.RegisterHealthServer(srv, hs)
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	reflection.Register(srv) // lets agentgateway probes / grpcurl introspect

	log.Printf("agentgateway-adapter: ext_authz v3 listening on %s -> PDP %s (fail_mode=%s, stage=%s)",
		cfg.Listen, cfg.AxonFlowEndpoint, cfg.FailMode, cfg.StageOr("llm"))

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		<-c
		log.Print("shutting down")
		srv.GracefulStop()
	}()

	if err := srv.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
