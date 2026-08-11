// Copyright 2026 AxonFlow
// SPDX-License-Identifier: BUSL-1.1
//
// MCP body governance for the ExtMcp shim (AID-212 follow-up). Pure logic: it
// parses an MCP JSON-RPC message, governs the tool arguments (request) or tool
// result (response) via the MCP PDP client, and returns a body decision the
// ext_proc wiring turns into a mutation or an immediate deny. No gRPC here, so
// it is fully unit-testable.
package adapter

import (
	"context"
	"encoding/json"
)

// BodyAction is what the ext_proc layer should do with the body.
type BodyAction int

const (
	ActionPass    BodyAction = iota // forward unchanged
	ActionReplace                   // forward NewBody instead
	ActionBlock                     // deny (immediate 403)
)

// BodyDecision is the outcome of governing one MCP body.
type BodyDecision struct {
	Action     BodyAction
	NewBody    []byte
	Reason     string
	DecisionID string
}

// jsonrpc is the minimal envelope we parse. Only tools/call requests and their
// results are governed; everything else passes through untouched.
type jsonrpc struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// GovernRequestBody governs an MCP request body. Only tools/call is inspected:
// the arguments are checked via check-input; a deny blocks, a redaction rewrites
// the arguments. Non-tool or unparsable bodies pass through.
func GovernRequestBody(ctx context.Context, mcp *MCPClient, body []byte) BodyDecision {
	var env jsonrpc
	if json.Unmarshal(body, &env) != nil || env.Method != "tools/call" || len(env.Params) == 0 {
		return BodyDecision{Action: ActionPass}
	}
	var p toolCallParams
	if json.Unmarshal(env.Params, &p) != nil {
		return BodyDecision{Action: ActionPass}
	}
	args := string(p.Arguments)
	if args == "" {
		return BodyDecision{Action: ActionPass}
	}

	v := mcp.CheckInput(ctx, p.Name, args)
	if !v.Allowed {
		return BodyDecision{Action: ActionBlock, Reason: nonEmptyReason(v.Reason, "tool call blocked by policy"), DecisionID: v.DecisionID}
	}
	if !v.WasRedacted {
		return BodyDecision{Action: ActionPass, DecisionID: v.DecisionID}
	}
	// Reinsert the masked arguments. If the redaction is valid JSON, use it
	// verbatim; otherwise wrap it so the body stays well-formed JSON-RPC.
	p.Arguments = asRawJSON(v.Redacted)
	np, _ := json.Marshal(p)
	env.Params = np
	nb, err := json.Marshal(env)
	if err != nil {
		// never forward the original once redaction was required — fail closed.
		return BodyDecision{Action: ActionBlock, Reason: "redaction rewrite failed (fail closed)", DecisionID: v.DecisionID}
	}
	return BodyDecision{Action: ActionReplace, NewBody: nb, DecisionID: v.DecisionID}
}

// GovernResponseBody governs an MCP tool result. agentgateway (v1.3.x) frames
// POST /mcp responses as SSE ("data: <json-rpc>" events), so SSE bodies are
// unwrapped per event; bare JSON bodies are governed directly. Text is pulled
// from the MCP content blocks (result.content[].text) when present, else the
// whole result; check-output decides block vs redact.
func GovernResponseBody(ctx context.Context, mcp *MCPClient, tool string, body []byte) BodyDecision {
	if looksLikeSSE(body) {
		return governSSEResponse(ctx, mcp, tool, body)
	}
	return governJSONResponse(ctx, mcp, tool, body)
}

// governJSONResponse governs a single bare JSON-RPC response body.
func governJSONResponse(ctx context.Context, mcp *MCPClient, tool string, body []byte) BodyDecision {
	var env jsonrpc
	if json.Unmarshal(body, &env) != nil || len(env.Result) == 0 {
		return BodyDecision{Action: ActionPass}
	}
	text, blocks := extractResultText(env.Result)
	if text == "" {
		return BodyDecision{Action: ActionPass}
	}
	v := mcp.CheckOutput(ctx, tool, text)
	if !v.Allowed {
		return BodyDecision{Action: ActionBlock, Reason: nonEmptyReason(v.Reason, "tool result blocked by policy"), DecisionID: v.DecisionID}
	}
	if !v.WasRedacted {
		return BodyDecision{Action: ActionPass, DecisionID: v.DecisionID}
	}
	nb, ok := rewriteResultText(env, blocks, v.Redacted)
	if !ok {
		return BodyDecision{Action: ActionBlock, Reason: "result redaction rewrite failed (fail closed)", DecisionID: v.DecisionID}
	}
	return BodyDecision{Action: ActionReplace, NewBody: nb, DecisionID: v.DecisionID}
}

// --- result text extraction / rewrite ---

type mcpResult struct {
	Content []mcpContent `json:"content,omitempty"`
}
type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// extractResultText returns the governable text of a tool result and whether it
// came from content blocks (so we can rewrite it in place).
func extractResultText(result json.RawMessage) (string, bool) {
	var r mcpResult
	if json.Unmarshal(result, &r) == nil && len(r.Content) > 0 {
		joined := ""
		for _, c := range r.Content {
			if c.Type == "text" {
				if joined != "" {
					joined += "\n"
				}
				joined += c.Text
			}
		}
		if joined != "" {
			return joined, true
		}
	}
	// fall back: govern the whole result JSON as text
	return string(result), false
}

// rewriteResultText puts the masked text back. When it came from content blocks
// we replace them with a single redacted text block; otherwise we replace the
// whole result with a redacted text result.
func rewriteResultText(env jsonrpc, fromBlocks bool, masked string) ([]byte, bool) {
	res := mcpResult{Content: []mcpContent{{Type: "text", Text: masked}}}
	rb, err := json.Marshal(res)
	if err != nil {
		return nil, false
	}
	env.Result = rb
	nb, err := json.Marshal(env)
	if err != nil {
		return nil, false
	}
	_ = fromBlocks
	return nb, true
}

func asRawJSON(s string) json.RawMessage {
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(map[string]string{"redacted": s})
	return json.RawMessage(b)
}

func nonEmptyReason(a, def string) string {
	if a != "" {
		return a
	}
	return def
}
