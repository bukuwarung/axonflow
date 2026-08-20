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
	"sync/atomic"
	"testing"
	"time"
)

// countingPDP is mockPDP plus a call counter, to pin the one-round-trip-per-
// request join optimisation.
func countingPDP(t *testing.T, calls *int64) *MCPClient {
	t.Helper()
	h := func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(calls, 1)
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
	return NewMCPClient(Config{AxonFlowEndpoint: srv.URL, OrgID: "t", LicenseKey: "k", RequestTimeout: 2 * time.Second}).WithConnector("llm")
}

func chatBody(contents ...string) []byte {
	msgs := []map[string]any{{"role": "system", "content": "be helpful"}}
	for _, c := range contents {
		msgs = append(msgs, map[string]any{"role": "user", "content": c})
	}
	b, _ := json.Marshal(map[string]any{
		"model": "gpt-4o", "temperature": 0.2, "stream": false, "messages": msgs,
	})
	return b
}

func TestGovernLLMRequest_CleanPasses(t *testing.T) {
	var calls int64
	d := GovernLLMRequestBody(context.Background(), countingPDP(t, &calls), chatBody("summarise the report"))
	if d.Action != ActionPass {
		t.Fatalf("action=%v, want pass", d.Action)
	}
	if calls != 1 {
		t.Fatalf("PDP calls=%d, want 1 (joined statement)", calls)
	}
}

func TestGovernLLMRequest_RedactsAcrossMessages(t *testing.T) {
	var calls int64
	d := GovernLLMRequestBody(context.Background(), countingPDP(t, &calls),
		chatBody("first SECRET here", "clean middle", "second SECRET there"))
	if d.Action != ActionReplace {
		t.Fatalf("action=%v, want replace", d.Action)
	}
	if calls != 1 {
		t.Fatalf("PDP calls=%d, want 1 (joined statement)", calls)
	}
	if strings.Contains(string(d.NewBody), "SECRET") {
		t.Fatal("REDACTION LEAK: original text survived in the rewritten body")
	}
	var out struct {
		Model       string  `json:"model"`
		Temperature float64 `json:"temperature"`
		Messages    []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(d.NewBody, &out); err != nil {
		t.Fatalf("rewritten body is not a chat request: %v", err)
	}
	if out.Model != "gpt-4o" || out.Temperature != 0.2 {
		t.Fatalf("envelope fields disturbed: %+v", out)
	}
	if len(out.Messages) != 4 || out.Messages[2].Content != "clean middle" {
		t.Fatalf("message structure disturbed: %+v", out.Messages)
	}
	if !strings.Contains(out.Messages[1].Content, "****") {
		t.Fatalf("first message not masked: %q", out.Messages[1].Content)
	}
}

func TestGovernLLMRequest_MultimodalParts(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"caption with SECRET"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	var calls int64
	d := GovernLLMRequestBody(context.Background(), countingPDP(t, &calls), body)
	if d.Action != ActionReplace {
		t.Fatalf("action=%v, want replace", d.Action)
	}
	s := string(d.NewBody)
	if strings.Contains(s, "SECRET") {
		t.Fatal("REDACTION LEAK in text part")
	}
	if !strings.Contains(s, "data:image/png;base64,AAAA") {
		t.Fatal("image part disturbed by the rewrite")
	}
}

func TestGovernLLMRequest_Blocks(t *testing.T) {
	var calls int64
	d := GovernLLMRequestBody(context.Background(), countingPDP(t, &calls), chatBody("BLOCKME now"))
	if d.Action != ActionBlock || d.DecisionID != "d-block" {
		t.Fatalf("action=%v decision=%q, want block/d-block", d.Action, d.DecisionID)
	}
}

func TestGovernLLMRequest_NonChatPasses(t *testing.T) {
	var calls int64
	d := GovernLLMRequestBody(context.Background(), countingPDP(t, &calls),
		[]byte(`{"model":"text-embedding-3-small","input":"embed even a SECRET"}`))
	if d.Action != ActionPass {
		t.Fatalf("action=%v, want pass for non-chat body", d.Action)
	}
	if calls != 0 {
		t.Fatalf("PDP calls=%d, want 0 for non-chat body", calls)
	}
}

func TestGovernLLMRequest_FailClosed(t *testing.T) {
	mcp := NewMCPClient(Config{AxonFlowEndpoint: "http://127.0.0.1:1", OrgID: "t", LicenseKey: "k", RequestTimeout: 300 * time.Millisecond})
	d := GovernLLMRequestBody(context.Background(), mcp, chatBody("anything"))
	if d.Action != ActionBlock {
		t.Fatalf("action=%v, want block when PDP is down (fail closed)", d.Action)
	}
}

func TestGovernLLMRequest_SeparatorInTextFallsBack(t *testing.T) {
	// A literal \x1e in one message defeats the join; the per-span fallback
	// must still govern each message (2 spans -> 2 calls).
	var calls int64
	d := GovernLLMRequestBody(context.Background(), countingPDP(t, &calls),
		chatBody("weird \x1e byte with SECRET", "also SECRET"))
	if d.Action != ActionReplace {
		t.Fatalf("action=%v, want replace", d.Action)
	}
	if strings.Contains(string(d.NewBody), "SECRET") {
		t.Fatal("REDACTION LEAK via separator fallback")
	}
	if calls != 3 { // 3 spans (system + 2 user), one call each
		t.Fatalf("PDP calls=%d, want 3 per-span fallback calls", calls)
	}
}

func TestGovernLLMResponseJSON_Redacts(t *testing.T) {
	body := []byte(`{"id":"c1","object":"chat.completion","model":"gpt-4o",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"answer with SECRET"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	var calls int64
	d := GovernLLMResponseJSON(context.Background(), countingPDP(t, &calls), "gpt-4o", body)
	if d.Action != ActionReplace {
		t.Fatalf("action=%v, want replace", d.Action)
	}
	s := string(d.NewBody)
	if strings.Contains(s, "SECRET") {
		t.Fatal("REDACTION LEAK in completion body")
	}
	if !strings.Contains(s, `"usage"`) || !strings.Contains(s, `"finish_reason"`) {
		t.Fatalf("completion envelope disturbed: %s", s)
	}
}

func TestGovernLLMResponseJSON_BlockAndPass(t *testing.T) {
	var calls int64
	pdp := countingPDP(t, &calls)
	block := GovernLLMResponseJSON(context.Background(), pdp, "m",
		[]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"BLOCKME"}}]}`))
	if block.Action != ActionBlock {
		t.Fatalf("action=%v, want block", block.Action)
	}
	pass := GovernLLMResponseJSON(context.Background(), pdp, "m",
		[]byte(`{"object":"list","data":[{"embedding":[0.1,0.2]}]}`))
	if pass.Action != ActionPass {
		t.Fatalf("action=%v, want pass for non-completion body", pass.Action)
	}
}
