// Copyright 2026 AxonFlow contributors
// SPDX-License-Identifier: BUSL-1.1

// Package adapter is an Envoy ext_authz v3 gRPC PEP that fronts the AxonFlow
// Enterprise Decision API. It is the buildable counterpart to the Enterprise
// axonflow-gateway-adapters binary described in
// docs.getaxonflow.com/docs/integration/agentgateway, for deployments that
// cannot obtain the Enterprise binary directly (AID-100).
//
// Scope: this build implements the ext_authz seam (request-plane
// authorisation + request-phase redact_pii obligations). The response-plane
// ext_proc seam and agentgateway's proprietary mcpGuardrails proto are
// intentionally out of scope here — see README.md.
package adapter

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config controls both the gRPC listener and the PDP HTTP calls. All fields
// are read from environment variables by LoadConfigFromEnv so the same
// container works in dev, staging, and prod without a config file.
type Config struct {
	// Listen is the gRPC bind address (default ":9090").
	Listen string

	// AxonFlowEndpoint is the base URL of the AxonFlow agent
	// (e.g. "https://axonflow-staging.example.com" or "http://localhost:8080").
	AxonFlowEndpoint string

	// OrgID / TenantID / GatewayID are stamped on every decide request so the
	// PDP can scope policy evaluation and audit rows.
	OrgID     string
	TenantID  string
	GatewayID string

	// LicenseKey is the Enterprise HTTP-Basic password (paired with OrgID).
	// Empty in Community mode; the adapter then makes anonymous decide calls.
	LicenseKey string

	// Stage is the AxonFlow decision stage — one of "llm", "tool", "agent"
	// (default "llm"). Set to "tool" on adapters fronting an MCP plane.
	Stage string

	// FailMode governs behavior when the PDP is unreachable or returns a
	// 5xx / transport error: "closed" (block, default) or "open" (forward).
	// 4xx errors from the PDP are NOT fail-mode eligible — those always block.
	FailMode string

	// RequestTimeout bounds each PDP decide call (default 10s).
	RequestTimeout time.Duration

	// MaxBodyBytes bounds how much of a request body the adapter will
	// forward to the PDP (default 8 MiB).
	MaxBodyBytes int

	// CompanionBodyRedaction (AXONFLOW_COMPANION_BODY_REDACTION=true) tells the
	// ext_authz adapter that a companion ext_proc shim on the SAME listener
	// rewrites every request body via check-input (AID-100 Fix B). Only then is
	// advertising request_body_redaction on /decide truthful, and only then may
	// a request-phase redact_pii obligation be treated as discharged. Setting
	// this WITHOUT the LLM extProc leg in the gateway config forwards
	// unredacted content — see the pdp_client.go capability warning.
	CompanionBodyRedaction bool
}

// FailOpen reports whether the configured fail-mode should forward on
// transport / 5xx failures.
func (c *Config) FailOpen() bool {
	return strings.EqualFold(c.FailMode, "open")
}

// StageOr returns Stage or the caller-supplied default.
func (c *Config) StageOr(def string) string {
	if c.Stage != "" {
		return c.Stage
	}
	return def
}

// LoadConfigFromEnv builds a Config from process env, applying defaults.
// It intentionally does not error on missing credentials — the community-mode
// PDP accepts anonymous calls, and a caller running against a stub PDP in
// tests should not have to set every var.
func LoadConfigFromEnv() Config {
	c := Config{
		Listen:           envOr("GATEWAY_ADAPTERS_LISTEN", ":9090"),
		AxonFlowEndpoint: envOr("AXONFLOW_ENDPOINT", "http://localhost:8080"),
		OrgID:            os.Getenv("AXONFLOW_ORG_ID"),
		TenantID:         os.Getenv("AXONFLOW_TENANT_ID"),
		GatewayID:        envOr("AXONFLOW_GATEWAY_ID", "agentgateway"),
		LicenseKey:       os.Getenv("AXONFLOW_LICENSE_KEY"),
		Stage:            envOr("AXONFLOW_STAGE", "llm"),
		FailMode:         envOr("AXONFLOW_FAIL_MODE", "closed"),
		RequestTimeout:   durationOr("AXONFLOW_REQUEST_TIMEOUT", 10*time.Second),
		MaxBodyBytes:     intOr("AXONFLOW_MAX_BODY_BYTES", 8*1024*1024),
		CompanionBodyRedaction: strings.EqualFold(
			os.Getenv("AXONFLOW_COMPANION_BODY_REDACTION"), "true"),
	}
	return c
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func intOr(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func durationOr(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
