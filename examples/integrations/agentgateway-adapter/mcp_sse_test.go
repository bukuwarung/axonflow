// Copyright 2026 AxonFlow
// SPDX-License-Identifier: BUSL-1.1
package adapter

import (
	"context"
	"strings"
	"testing"
)

const sseClean = "id: 7\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"plain output\"}]}}\n\n"

func TestLooksLikeSSE(t *testing.T) {
	if !looksLikeSSE([]byte(sseClean)) {
		t.Fatal("SSE body not detected")
	}
	if looksLikeSSE([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)) {
		t.Fatal("bare JSON misdetected as SSE")
	}
}

func TestGovernResponseSSE_CleanPasses(t *testing.T) {
	d := GovernResponseBody(context.Background(), mockPDP(t), "Bash", []byte(sseClean))
	if d.Action != ActionPass {
		t.Fatalf("action=%v, want pass", d.Action)
	}
}

func TestGovernResponseSSE_RedactsAndPreservesFraming(t *testing.T) {
	body := "id: 7\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"leaked SECRET value\"}]}}\n\n"
	d := GovernResponseBody(context.Background(), mockPDP(t), "Bash", []byte(body))
	if d.Action != ActionReplace {
		t.Fatalf("action=%v, want replace", d.Action)
	}
	out := string(d.NewBody)
	if strings.Contains(out, "SECRET") {
		t.Fatal("REDACTION LEAK in SSE body")
	}
	if !strings.HasPrefix(out, "id: 7\n") {
		t.Fatalf("event id line not preserved: %q", out)
	}
	if !strings.Contains(out, "data: {") || !strings.HasSuffix(out, "\n\n") {
		t.Fatalf("re-framed body is not valid SSE: %q", out)
	}
}

func TestGovernResponseSSE_Blocks(t *testing.T) {
	body := "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"BLOCKME output\"}]}}\n\n"
	d := GovernResponseBody(context.Background(), mockPDP(t), "Bash", []byte(body))
	if d.Action != ActionBlock {
		t.Fatalf("action=%v, want block", d.Action)
	}
}

func TestGovernResponseSSE_MultiEventOnlyGovernedRewritten(t *testing.T) {
	body := "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"has SECRET here\"}]}}\n\n"
	d := GovernResponseBody(context.Background(), mockPDP(t), "Bash", []byte(body))
	if d.Action != ActionReplace {
		t.Fatalf("action=%v, want replace", d.Action)
	}
	out := string(d.NewBody)
	if !strings.Contains(out, "notifications/progress") {
		t.Fatal("ungoverned notification event was dropped")
	}
	if strings.Contains(out, "SECRET") {
		t.Fatal("REDACTION LEAK in multi-event SSE body")
	}
	if got := strings.Count(out, "data: "); got != 2 {
		t.Fatalf("expected 2 events, got %d: %q", got, out)
	}
}

func TestGovernResponseSSE_NoResultPasses(t *testing.T) {
	body := ": keep-alive\n\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n"
	d := GovernResponseBody(context.Background(), mockPDP(t), "Bash", []byte(body))
	if d.Action != ActionPass {
		t.Fatalf("action=%v, want pass", d.Action)
	}
}

func TestGovernResponseSSE_CRLFTolerated(t *testing.T) {
	body := "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"has SECRET here\"}]}}\r\n\r\n"
	d := GovernResponseBody(context.Background(), mockPDP(t), "Bash", []byte(body))
	if d.Action != ActionReplace {
		t.Fatalf("action=%v, want replace", d.Action)
	}
	if strings.Contains(string(d.NewBody), "SECRET") {
		t.Fatal("REDACTION LEAK with CRLF framing")
	}
}

func TestBodyResponse_ReplaceKeepsContentLengthConsistent(t *testing.T) {
	s := NewExtMcpServer(Config{}, nil)
	nb := []byte(`{"jsonrpc":"2.0"}`)
	// request phase: content-length must be SET to the new body length
	pr := s.bodyResponse(BodyDecision{Action: ActionReplace, NewBody: nb}, true)
	cr := pr.GetRequestBody().GetResponse()
	if cr.GetHeaderMutation() == nil || len(cr.GetHeaderMutation().GetSetHeaders()) != 1 {
		t.Fatal("request replace must set content-length")
	}
	h := cr.GetHeaderMutation().GetSetHeaders()[0].GetHeader()
	if h.GetKey() != "content-length" || string(h.GetRawValue()) != "17" {
		t.Fatalf("bad header mutation: %s=%s", h.GetKey(), h.GetRawValue())
	}
	// response phase: content-length must be REMOVED
	pr = s.bodyResponse(BodyDecision{Action: ActionReplace, NewBody: nb}, false)
	cr = pr.GetResponseBody().GetResponse()
	rm := cr.GetHeaderMutation().GetRemoveHeaders()
	if len(rm) != 1 || rm[0] != "content-length" {
		t.Fatalf("response replace must remove content-length, got %v", rm)
	}
}
