// Copyright 2026 AxonFlow
// SPDX-License-Identifier: BUSL-1.1
package adapter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mockPDP routes check-input / check-output and reacts to markers in the
// governed text, so the tests carry no literal PII/SQL.
func mockPDP(t *testing.T) *MCPClient {
	t.Helper()
	h := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Statement string `json:"statement"`
			Message   string `json:"message"`
		}
		_ = json.Unmarshal(body, &req)
		text := req.Statement + req.Message
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(text, "BLOCKME"):
			_, _ = w.Write([]byte(`{"allowed":false,"block_reason":"blocked by policy","decision_id":"d-block"}`))
		case strings.Contains(text, "SECRET"):
			masked := strings.ReplaceAll(text, "SECRET", "****")
			resp := map[string]any{"allowed": true, "redaction_evaluated": true, "decision_id": "d-redact"}
			if strings.Contains(r.URL.Path, "check-input") {
				resp["redacted_statement"] = masked
			} else {
				resp["redacted_data"] = masked
			}
			b, _ := json.Marshal(resp)
			_, _ = w.Write(b)
		default:
			_, _ = w.Write([]byte(`{"allowed":true,"redaction_evaluated":true,"decision_id":"d-ok"}`))
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(h))
	t.Cleanup(srv.Close)
	return NewMCPClient(Config{AxonFlowEndpoint: srv.URL, OrgID: "t", LicenseKey: "k", RequestTimeout: 2 * time.Second})
}

func TestGovernRequest_CleanPasses(t *testing.T) {
	d := GovernRequestBody(context.Background(), mockPDP(t),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"Bash","arguments":{"command":"ls"}}}`))
	if d.Action != ActionPass {
		t.Fatalf("action=%v, want pass", d.Action)
	}
}

func TestGovernRequest_Blocks(t *testing.T) {
	d := GovernRequestBody(context.Background(), mockPDP(t),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"Bash","arguments":{"command":"BLOCKME now"}}}`))
	if d.Action != ActionBlock {
		t.Fatalf("action=%v, want block", d.Action)
	}
	if d.DecisionID != "d-block" {
		t.Fatalf("decision=%q", d.DecisionID)
	}
}

func TestGovernRequest_Redacts(t *testing.T) {
	d := GovernRequestBody(context.Background(), mockPDP(t),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"db","arguments":{"q":"token SECRET here"}}}`))
	if d.Action != ActionReplace {
		t.Fatalf("action=%v, want replace", d.Action)
	}
	if strings.Contains(string(d.NewBody), "SECRET") {
		t.Fatal("REDACTION LEAK: original secret survived in the rewritten body")
	}
	// still valid JSON-RPC tools/call
	var env map[string]any
	if json.Unmarshal(d.NewBody, &env) != nil || env["method"] != "tools/call" {
		t.Fatalf("rewritten body is not valid tools/call: %s", d.NewBody)
	}
}

func TestGovernRequest_NonToolPasses(t *testing.T) {
	d := GovernRequestBody(context.Background(), mockPDP(t),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if d.Action != ActionPass {
		t.Fatalf("action=%v, want pass for non-tool", d.Action)
	}
}

func TestGovernRequest_FailClosed(t *testing.T) {
	// point the client at a dead endpoint -> check-input unavailable -> block.
	mcp := NewMCPClient(Config{AxonFlowEndpoint: "http://127.0.0.1:1", OrgID: "t", LicenseKey: "k", RequestTimeout: 300 * time.Millisecond})
	d := GovernRequestBody(context.Background(), mcp,
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"Bash","arguments":{"command":"ls"}}}`))
	if d.Action != ActionBlock {
		t.Fatalf("action=%v, want block when PDP is down (fail closed)", d.Action)
	}
}

func TestGovernResponse_RedactsResult(t *testing.T) {
	d := GovernResponseBody(context.Background(), mockPDP(t), "Bash",
		[]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"leaked SECRET value"}]}}`))
	if d.Action != ActionReplace {
		t.Fatalf("action=%v, want replace", d.Action)
	}
	if strings.Contains(string(d.NewBody), "SECRET") {
		t.Fatal("REDACTION LEAK in result body")
	}
}

func TestGovernResponse_Blocks(t *testing.T) {
	d := GovernResponseBody(context.Background(), mockPDP(t), "Bash",
		[]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"BLOCKME output"}]}}`))
	if d.Action != ActionBlock {
		t.Fatalf("action=%v, want block", d.Action)
	}
}
