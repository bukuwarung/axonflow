// Copyright 2026 AxonFlow
// SPDX-License-Identifier: BUSL-1.1
//
// LLM body governance for the ext_proc LLM shim (AID-100 Fix B). Pure logic:
// it parses an OpenAI chat-completions envelope, governs the message text
// (request) or the completion text (response) via the MCP PDP client's
// check-input / check-output planes, and returns a body decision the ext_proc
// wiring turns into a mutation or an immediate deny. No gRPC here, so it is
// fully unit-testable — the same split as mcp_govern.go.
//
// Only chat-completions bodies are governed: anything without a messages[]
// array (embeddings, rerank, admin calls) passes through untouched, exactly
// like non-tools/call JSON-RPC passes through the MCP shim.
package adapter

import (
	"context"
	"encoding/json"
	"strings"
)

// chatTextSep joins the per-message text spans into ONE check-input statement
// so a request costs one PDP round trip instead of one per message. The engine
// masks spans in place and never touches non-matching bytes, so splitting the
// redacted statement on the same separator recovers the per-message spans.
// \x1e (ASCII record separator) never occurs in normal chat text; if it does —
// or if a redaction ate a separator (span straddling two messages) — the
// split-back count mismatches and we fall back to one PDP call per span.
const chatTextSep = "\n\x1e\n"

// llmTextRef locates one governable text span inside a chat-completions body:
// message msgIdx, and within it either the whole string content (partIdx < 0)
// or content part partIdx (multimodal content arrays; only "text" parts carry
// governable text — image parts pass through untouched).
type llmTextRef struct {
	msgIdx  int
	partIdx int
	text    string
}

// chatRequest is the decoded-enough view of a chat-completions request:
// the envelope and messages keep every unknown field as raw JSON so the
// rewrite disturbs nothing but the governed text.
type chatRequest struct {
	envelope json.RawMessage            // original body
	fields   map[string]json.RawMessage // top-level fields
	messages []map[string]json.RawMessage
	refs     []llmTextRef
	model    string
	stream   bool
}

// parseChatRequest decodes body as a chat-completions request. ok is false —
// meaning "not governable, pass through" — when the body is not JSON, has no
// messages array, or carries no text.
func parseChatRequest(body []byte) (*chatRequest, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return nil, false
	}
	rawMsgs, has := fields["messages"]
	if !has {
		return nil, false
	}
	var msgList []json.RawMessage
	if json.Unmarshal(rawMsgs, &msgList) != nil {
		return nil, false
	}
	cr := &chatRequest{envelope: body, fields: fields}
	if m, ok := fields["model"]; ok {
		_ = json.Unmarshal(m, &cr.model)
	}
	if s, ok := fields["stream"]; ok {
		_ = json.Unmarshal(s, &cr.stream)
	}
	for i, raw := range msgList {
		var msg map[string]json.RawMessage
		if json.Unmarshal(raw, &msg) != nil {
			return nil, false // malformed message: do not half-govern the body
		}
		cr.messages = append(cr.messages, msg)
		content, has := msg["content"]
		if !has {
			continue
		}
		var s string
		if json.Unmarshal(content, &s) == nil {
			if s != "" {
				cr.refs = append(cr.refs, llmTextRef{msgIdx: i, partIdx: -1, text: s})
			}
			continue
		}
		// content parts array (multimodal): govern each text part in place.
		var parts []map[string]json.RawMessage
		if json.Unmarshal(content, &parts) != nil {
			continue // null / unknown shape: nothing to govern here
		}
		for pi, part := range parts {
			var ptype, ptext string
			if t, ok := part["type"]; ok {
				_ = json.Unmarshal(t, &ptype)
			}
			if ptype != "text" {
				continue
			}
			if t, ok := part["text"]; ok && json.Unmarshal(t, &ptext) == nil && ptext != "" {
				cr.refs = append(cr.refs, llmTextRef{msgIdx: i, partIdx: pi, text: ptext})
			}
		}
	}
	if len(cr.refs) == 0 {
		return nil, false
	}
	return cr, true
}

// rewrite puts masked span texts back into the request and re-marshals it.
// masked must be parallel to cr.refs.
func (cr *chatRequest) rewrite(masked []string) ([]byte, bool) {
	// Group part-level edits per message so a message's content array is
	// decoded and re-encoded once.
	type partEdit struct {
		partIdx int
		text    string
	}
	partEdits := map[int][]partEdit{}
	for i, ref := range cr.refs {
		if masked[i] == ref.text {
			continue
		}
		if ref.partIdx < 0 {
			nb, err := json.Marshal(masked[i])
			if err != nil {
				return nil, false
			}
			cr.messages[ref.msgIdx]["content"] = nb
			continue
		}
		partEdits[ref.msgIdx] = append(partEdits[ref.msgIdx], partEdit{ref.partIdx, masked[i]})
	}
	for msgIdx, edits := range partEdits {
		var parts []map[string]json.RawMessage
		if json.Unmarshal(cr.messages[msgIdx]["content"], &parts) != nil {
			return nil, false
		}
		for _, e := range edits {
			nb, err := json.Marshal(e.text)
			if err != nil {
				return nil, false
			}
			parts[e.partIdx]["text"] = nb
		}
		nb, err := json.Marshal(parts)
		if err != nil {
			return nil, false
		}
		cr.messages[msgIdx]["content"] = nb
	}
	msgList := make([]json.RawMessage, len(cr.messages))
	for i, m := range cr.messages {
		nb, err := json.Marshal(m)
		if err != nil {
			return nil, false
		}
		msgList[i] = nb
	}
	nb, err := json.Marshal(msgList)
	if err != nil {
		return nil, false
	}
	cr.fields["messages"] = nb
	out, err := json.Marshal(cr.fields)
	if err != nil {
		return nil, false
	}
	return out, true
}

// governTexts runs the span texts through check via ONE joined call, falling
// back to one call per span when the join is not splittable (see chatTextSep).
// Returns the masked texts (parallel to texts; equal strings when untouched)
// plus the decision id for audit correlation, or a non-nil block decision.
func governTexts(ctx context.Context, texts []string, check func(context.Context, string) MCPVerdict) ([]string, string, *BodyDecision) {
	joinable := true
	for _, t := range texts {
		if strings.Contains(t, "\x1e") {
			joinable = false
			break
		}
	}
	if joinable && len(texts) > 1 {
		v := check(ctx, strings.Join(texts, chatTextSep))
		if !v.Allowed {
			return nil, v.DecisionID, &BodyDecision{Action: ActionBlock, Reason: nonEmptyReason(v.Reason, "request blocked by policy"), DecisionID: v.DecisionID}
		}
		if !v.WasRedacted {
			return texts, v.DecisionID, nil
		}
		pieces := strings.Split(v.Redacted, chatTextSep)
		if len(pieces) == len(texts) {
			return pieces, v.DecisionID, nil
		}
		// A redaction span straddled a separator — rare, but the per-span
		// fallback below still governs everything correctly.
	}
	out := make([]string, len(texts))
	lastDecision := ""
	for i, t := range texts {
		v := check(ctx, t)
		if v.DecisionID != "" {
			lastDecision = v.DecisionID
		}
		if !v.Allowed {
			return nil, v.DecisionID, &BodyDecision{Action: ActionBlock, Reason: nonEmptyReason(v.Reason, "request blocked by policy"), DecisionID: v.DecisionID}
		}
		if v.WasRedacted {
			out[i] = v.Redacted
		} else {
			out[i] = t
		}
	}
	return out, lastDecision, nil
}

// GovernLLMRequestBody governs a chat-completions request body. Message text
// (string content and text content-parts, every role) is checked via
// check-input; a deny blocks, a redaction rewrites the text in place. Bodies
// that are not chat-completions pass through.
func GovernLLMRequestBody(ctx context.Context, mcp *MCPClient, body []byte) BodyDecision {
	cr, ok := parseChatRequest(body)
	if !ok {
		return BodyDecision{Action: ActionPass}
	}
	texts := make([]string, len(cr.refs))
	for i, r := range cr.refs {
		texts[i] = r.text
	}
	masked, decisionID, blocked := governTexts(ctx, texts, func(ctx context.Context, s string) MCPVerdict {
		return mcp.CheckInput(ctx, cr.model, s)
	})
	if blocked != nil {
		return *blocked
	}
	changed := false
	for i := range texts {
		if masked[i] != texts[i] {
			changed = true
			break
		}
	}
	if !changed {
		return BodyDecision{Action: ActionPass, DecisionID: decisionID}
	}
	nb, ok := cr.rewrite(masked)
	if !ok {
		// never forward the original once redaction was required — fail closed.
		return BodyDecision{Action: ActionBlock, Reason: "request redaction rewrite failed (fail closed)", DecisionID: decisionID}
	}
	return BodyDecision{Action: ActionReplace, NewBody: nb, DecisionID: decisionID}
}

// GovernLLMResponseJSON governs a NON-streaming chat-completions response body
// (choices[].message.content) via check-output. Streaming SSE responses go
// through LLMStreamGovernor instead. Bodies that are not chat completions pass.
func GovernLLMResponseJSON(ctx context.Context, mcp *MCPClient, model string, body []byte) BodyDecision {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return BodyDecision{Action: ActionPass}
	}
	rawChoices, has := fields["choices"]
	if !has {
		return BodyDecision{Action: ActionPass}
	}
	var choices []map[string]json.RawMessage
	if json.Unmarshal(rawChoices, &choices) != nil {
		return BodyDecision{Action: ActionPass}
	}
	type ref struct {
		choice int
		text   string
	}
	var refs []ref
	msgs := make([]map[string]json.RawMessage, len(choices))
	for i, c := range choices {
		rawMsg, has := c["message"]
		if !has {
			continue
		}
		var msg map[string]json.RawMessage
		if json.Unmarshal(rawMsg, &msg) != nil {
			continue
		}
		msgs[i] = msg
		var s string
		if content, has := msg["content"]; has && json.Unmarshal(content, &s) == nil && s != "" {
			refs = append(refs, ref{choice: i, text: s})
		}
	}
	if len(refs) == 0 {
		return BodyDecision{Action: ActionPass}
	}
	texts := make([]string, len(refs))
	for i, r := range refs {
		texts[i] = r.text
	}
	masked, decisionID, blocked := governTexts(ctx, texts, func(ctx context.Context, s string) MCPVerdict {
		return mcp.CheckOutput(ctx, model, s)
	})
	if blocked != nil {
		return *blocked
	}
	changed := false
	for i, r := range refs {
		if masked[i] == r.text {
			continue
		}
		changed = true
		nb, err := json.Marshal(masked[i])
		if err != nil {
			return BodyDecision{Action: ActionBlock, Reason: "response redaction rewrite failed (fail closed)", DecisionID: decisionID}
		}
		msgs[r.choice]["content"] = nb
	}
	if !changed {
		return BodyDecision{Action: ActionPass, DecisionID: decisionID}
	}
	for i, msg := range msgs {
		if msg == nil {
			continue
		}
		nb, err := json.Marshal(msg)
		if err != nil {
			return BodyDecision{Action: ActionBlock, Reason: "response redaction rewrite failed (fail closed)", DecisionID: decisionID}
		}
		choices[i]["message"] = nb
	}
	nb, err := json.Marshal(choices)
	if err != nil {
		return BodyDecision{Action: ActionBlock, Reason: "response redaction rewrite failed (fail closed)", DecisionID: decisionID}
	}
	fields["choices"] = nb
	out, err := json.Marshal(fields)
	if err != nil {
		return BodyDecision{Action: ActionBlock, Reason: "response redaction rewrite failed (fail closed)", DecisionID: decisionID}
	}
	return BodyDecision{Action: ActionReplace, NewBody: out, DecisionID: decisionID}
}
