// Copyright 2026 AxonFlow
// SPDX-License-Identifier: BUSL-1.1
//
// MCP PDP client for the ExtMcp shim (AID-212 follow-up). Governs MCP tool
// args/results through the AxonFlow agent's CORE endpoints — no Enterprise
// feature:
//
//	POST /api/v1/mcp/check-input   — request phase (tool arguments)
//	POST /api/v1/mcp/check-output  — response phase (tool results)
//
// Both are the same endpoints the Claude Code / Desktop plugin already uses;
// verified present and working on the Community (BSL) build.
package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// MCPClient calls the check-input / check-output planes.
type MCPClient struct {
	cfg       Config
	http      *http.Client
	connector string // connector_type label on PDP calls; default "mcp"
}

// NewMCPClient builds an MCP PDP client from the shared adapter Config.
func NewMCPClient(cfg Config) *MCPClient {
	return &MCPClient{cfg: cfg, http: &http.Client{Timeout: cfg.RequestTimeout}, connector: "mcp"}
}

// WithConnector returns a copy labelling its PDP calls with the given
// connector_type — the LLM shim uses "llm" so audit rows and per-connector
// policy scoping distinguish the two planes. The check-input/check-output
// wire contract is otherwise plane-agnostic (text in, redacted text out).
func (c *MCPClient) WithConnector(name string) *MCPClient {
	cp := *c
	if name != "" {
		cp.connector = name
	}
	return &cp
}

func (c *MCPClient) connectorType() string {
	if c.connector != "" {
		return c.connector
	}
	return "mcp"
}

// MCPVerdict is the normalized outcome of a check-input / check-output call.
type MCPVerdict struct {
	Allowed     bool   // false ⇒ block the tool call
	Redacted    string // masked content to substitute (empty ⇒ no change)
	WasRedacted bool   // engine actually masked something
	DecisionID  string
	Evaluated   bool // the redactor ran; false ⇒ fail closed (do not trust "clean")
	Reason      string
}

type checkInputReq struct {
	ClientID      string `json:"client_id"`
	TenantID      string `json:"tenant_id"`
	ConnectorType string `json:"connector_type"`
	Tool          string `json:"tool,omitempty"`
	Operation     string `json:"operation,omitempty"`
	Statement     string `json:"statement"`
}

type checkInputResp struct {
	Allowed            bool   `json:"allowed"`
	BlockReason        string `json:"block_reason"`
	RedactedStatement  string `json:"redacted_statement"`
	RedactionEvaluated bool   `json:"redaction_evaluated"`
	DecisionID         string `json:"decision_id"`
}

type checkOutputReq struct {
	ClientID      string `json:"client_id"`
	TenantID      string `json:"tenant_id"`
	ConnectorType string `json:"connector_type"`
	Tool          string `json:"tool,omitempty"`
	Message       string `json:"message"`
}

type checkOutputResp struct {
	Allowed            bool        `json:"allowed"`
	BlockReason        string      `json:"block_reason"`
	RedactedData       interface{} `json:"redacted_data"`
	RedactionEvaluated bool        `json:"redaction_evaluated"`
	DecisionID         string      `json:"decision_id"`
}

// CheckInput governs the tool arguments (request phase).
func (c *MCPClient) CheckInput(ctx context.Context, tool, args string) MCPVerdict {
	body, _ := json.Marshal(checkInputReq{
		ClientID: c.cfg.OrgID, TenantID: firstNonEmpty(c.cfg.TenantID, c.cfg.OrgID),
		ConnectorType: c.connectorType(), Tool: tool, Operation: "execute", Statement: args,
	})
	var r checkInputResp
	if !c.call(ctx, "/api/v1/mcp/check-input", body, &r) {
		return MCPVerdict{Allowed: false, Reason: "PDP check-input unavailable (fail closed)"}
	}
	v := MCPVerdict{Allowed: r.Allowed, DecisionID: r.DecisionID, Evaluated: r.RedactionEvaluated, Reason: r.BlockReason}
	if r.Allowed && r.RedactionEvaluated && r.RedactedStatement != "" && r.RedactedStatement != args {
		v.Redacted, v.WasRedacted = r.RedactedStatement, true
	}
	return v
}

// CheckOutput governs the tool result (response phase).
func (c *MCPClient) CheckOutput(ctx context.Context, tool, result string) MCPVerdict {
	body, _ := json.Marshal(checkOutputReq{
		ClientID: c.cfg.OrgID, TenantID: firstNonEmpty(c.cfg.TenantID, c.cfg.OrgID),
		ConnectorType: c.connectorType(), Tool: tool, Message: result,
	})
	var r checkOutputResp
	if !c.call(ctx, "/api/v1/mcp/check-output", body, &r) {
		return MCPVerdict{Allowed: false, Reason: "PDP check-output unavailable (fail closed)"}
	}
	v := MCPVerdict{Allowed: r.Allowed, DecisionID: r.DecisionID, Evaluated: r.RedactionEvaluated, Reason: r.BlockReason}
	masked := redactedToString(r.RedactedData)
	if r.Allowed && r.RedactionEvaluated && masked != "" && masked != result {
		v.Redacted, v.WasRedacted = masked, true
	}
	return v
}

// call POSTs body to path and decodes into out. Returns false on any transport
// error or non-2xx — the caller then fails closed.
func (c *MCPClient) call(ctx context.Context, path string, body []byte, out interface{}) bool {
	url := strings.TrimRight(c.cfg.AxonFlowEndpoint, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.OrgID != "" && c.cfg.LicenseKey != "" {
		req.SetBasicAuth(c.cfg.OrgID, c.cfg.LicenseKey)
	}
	if c.cfg.TenantID != "" {
		req.Header.Set("X-Tenant-ID", c.cfg.TenantID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	return json.Unmarshal(payload, out) == nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// redactedToString normalizes the polymorphic redacted_data (string or rows).
func redactedToString(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(b)
	}
}
