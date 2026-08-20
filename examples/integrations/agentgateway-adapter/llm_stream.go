// Copyright 2026 AxonFlow
// SPDX-License-Identifier: BUSL-1.1
//
// Streaming SSE governor for the LLM shim's response plane (AID-100 Fix B).
// agentgateway forwards streamed chat completions as SSE chunk events
// ("data: {...chat.completion.chunk...}"); this governor re-times that stream
// so text is governed BEFORE the client sees it, without buffering the whole
// response:
//
//   - delta text accumulates in a pending window and is released in governed
//     SEGMENTS (one check-output call per segment, not per token-chunk);
//   - a TAIL of the pending text is always held back so a PII span straddling
//     a segment boundary is governed whole — a per-chunk regex would let a
//     split NIK/salary walk straight through (the brief's chunk-boundary trap);
//   - non-content events (role prelude, finish_reason, usage, [DONE]) force a
//     full flush first, so event ORDER is preserved for the client.
//
// The governed text is re-emitted as synthetic chunk events cloned from the
// last content-bearing chunk, so id/model/object stay consistent. Collapsing
// many token-deltas into fewer, larger deltas is protocol-legal — clients
// concatenate delta.content.
//
// Pure logic, no gRPC: Feed/Close return the bytes to emit downstream, and
// governance is injected as a callback, so the whole flow is unit-testable.
package adapter

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// GovernTextFunc governs one text segment. masked is the (possibly identical)
// replacement text; blocked=true withholds the rest of the stream.
type GovernTextFunc func(text string) (masked string, blocked bool, reason string)

const (
	// defaultSegmentBytes is how much pending text triggers a mid-stream
	// governed release. Small enough to keep the stream feeling live, large
	// enough that check-output round trips stay ~1 per few hundred tokens.
	defaultSegmentBytes = 1536
	// defaultTailBytes is the holdback window. It must exceed the longest
	// redactable span (labelled salary lines, NIK/NPWP, card numbers are all
	// well under 100 bytes); 256 leaves margin.
	defaultTailBytes = 256
)

// LLMStreamGovernor governs one SSE response stream.
type LLMStreamGovernor struct {
	govern       GovernTextFunc
	segmentBytes int
	tailBytes    int

	raw      []byte                     // incoming bytes not yet split into complete events
	pending  string                     // accumulated, not-yet-governed delta text
	template map[string]json.RawMessage // last content-bearing chunk, for synthetic events
	choice   int                        // choice index the pending text belongs to
	blocked  bool                       // a segment was blocked: drop all further content
}

// NewLLMStreamGovernor builds a governor with default segment/tail sizing.
func NewLLMStreamGovernor(govern GovernTextFunc) *LLMStreamGovernor {
	return &LLMStreamGovernor{govern: govern, segmentBytes: defaultSegmentBytes, tailBytes: defaultTailBytes}
}

// Feed consumes the next chunk of SSE bytes and returns whatever is safe to
// emit downstream now (possibly nothing while text is held back).
func (g *LLMStreamGovernor) Feed(chunk []byte) []byte {
	g.raw = append(g.raw, chunk...)
	var out bytes.Buffer
	for {
		ev, rest, ok := nextSSEEvent(g.raw)
		if !ok {
			break
		}
		g.raw = rest
		out.Write(g.consumeEvent(ev))
	}
	return out.Bytes()
}

// Close flushes everything at end of stream: any complete-but-unemitted
// events, the pending text (governed in full, tail included), and any raw
// remainder that never became a complete event (e.g. a stream cut mid-event).
func (g *LLMStreamGovernor) Close() []byte {
	var out bytes.Buffer
	for {
		ev, rest, ok := nextSSEEvent(g.raw)
		if !ok {
			break
		}
		g.raw = rest
		out.Write(g.consumeEvent(ev))
	}
	out.Write(g.flushAll())
	// A trailing fragment without its blank-line terminator: too small to
	// govern as text (no data payload parsed), forward verbatim rather than
	// truncate the stream.
	out.Write(g.raw)
	g.raw = nil
	return out.Bytes()
}

// consumeEvent routes one complete SSE event.
func (g *LLMStreamGovernor) consumeEvent(ev []byte) []byte {
	payload, hasData := eventData(ev)
	if !hasData || strings.TrimSpace(payload) == "[DONE]" {
		return append(g.flushAll(), normalizeEvent(ev)...)
	}
	chunk, content, choiceIdx, isContent := parseChunkContent(payload)
	if !isContent {
		// role prelude / finish_reason / usage / non-chunk JSON: order matters,
		// so release everything governed first, then the event itself.
		return append(g.flushAll(), normalizeEvent(ev)...)
	}
	if g.blocked {
		return nil // withhold all further content after a block
	}
	var out bytes.Buffer
	if g.template != nil && choiceIdx != g.choice {
		// n>1 interleave: never collapse text across choices.
		out.Write(g.flushAll())
	}
	g.template, g.choice = chunk, choiceIdx
	g.pending += content
	if len(g.pending) >= g.segmentBytes+g.tailBytes {
		out.Write(g.flushSegment())
	}
	return out.Bytes()
}

// flushSegment governs and emits the pending text minus the tail holdback.
func (g *LLMStreamGovernor) flushSegment() []byte {
	cut := len(g.pending) - g.tailBytes
	// never split a UTF-8 rune between two synthetic events
	for cut > 0 && !utf8.RuneStart(g.pending[cut]) {
		cut--
	}
	if cut <= 0 {
		return nil
	}
	segment := g.pending[:cut]
	g.pending = g.pending[cut:]
	return g.emitGoverned(segment)
}

// flushAll governs and emits all pending text, tail included.
func (g *LLMStreamGovernor) flushAll() []byte {
	if g.pending == "" {
		return nil
	}
	segment := g.pending
	g.pending = ""
	return g.emitGoverned(segment)
}

func (g *LLMStreamGovernor) emitGoverned(segment string) []byte {
	masked, blocked, reason := g.govern(segment)
	if blocked {
		g.blocked = true
		notice := "\n[remaining output withheld by AxonFlow policy"
		if reason != "" {
			notice += ": " + reason
		}
		notice += "]"
		return g.syntheticEvent(notice)
	}
	return g.syntheticEvent(masked)
}

// syntheticEvent clones the template chunk with delta.content set to text.
func (g *LLMStreamGovernor) syntheticEvent(text string) []byte {
	if g.template == nil {
		return nil
	}
	chunk := make(map[string]json.RawMessage, len(g.template))
	for k, v := range g.template {
		chunk[k] = v
	}
	var choices []map[string]json.RawMessage
	if json.Unmarshal(chunk["choices"], &choices) != nil || len(choices) == 0 {
		return nil
	}
	var delta map[string]json.RawMessage
	if json.Unmarshal(choices[0]["delta"], &delta) != nil {
		delta = map[string]json.RawMessage{}
	}
	nb, err := json.Marshal(text)
	if err != nil {
		return nil
	}
	delta["content"] = nb
	db, err := json.Marshal(delta)
	if err != nil {
		return nil
	}
	choices[0]["delta"] = db
	cb, err := json.Marshal(choices)
	if err != nil {
		return nil
	}
	chunk["choices"] = cb
	eb, err := json.Marshal(chunk)
	if err != nil {
		return nil
	}
	return []byte("data: " + string(eb) + "\n\n")
}

// --- SSE event scanning (incremental; mcp_sse.go parses whole bodies only) ---

// nextSSEEvent splits the first complete event (terminated by a blank line)
// off buf. ok is false when no complete event is buffered yet.
func nextSSEEvent(buf []byte) (event, rest []byte, ok bool) {
	for _, sep := range [][]byte{[]byte("\n\n"), []byte("\r\n\r\n")} {
		if i := bytes.Index(buf, sep); i >= 0 {
			// pick the EARLIEST terminator of either style
			if j := bytes.Index(buf, []byte("\r\n\r\n")); sep[0] == '\n' && len(sep) == 2 && j >= 0 && j < i {
				continue
			}
			return buf[:i+len(sep)], buf[i+len(sep):], true
		}
	}
	return nil, buf, false
}

// eventData extracts and joins the event's data lines (SSE: multiple data
// lines concatenate with \n).
func eventData(event []byte) (string, bool) {
	var parts []string
	for _, raw := range bytes.Split(event, []byte("\n")) {
		line := strings.TrimRight(string(raw), "\r")
		if strings.HasPrefix(line, "data:") {
			parts = append(parts, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n"), true
}

// normalizeEvent re-emits a passthrough event with \n line endings and its
// blank-line terminator intact.
func normalizeEvent(event []byte) []byte {
	s := strings.ReplaceAll(string(event), "\r\n", "\n")
	if !strings.HasSuffix(s, "\n\n") {
		s = strings.TrimRight(s, "\n") + "\n\n"
	}
	return []byte(s)
}

// parseChunkContent decodes one data payload. isContent is true only for a
// chat.completion.chunk whose first choice delta carries non-empty content
// and no finish_reason — the events that are safe to collapse and re-time.
func parseChunkContent(payload string) (chunk map[string]json.RawMessage, content string, choiceIdx int, isContent bool) {
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return nil, "", 0, false
	}
	rawChoices, has := chunk["choices"]
	if !has {
		return nil, "", 0, false
	}
	var choices []map[string]json.RawMessage
	if json.Unmarshal(rawChoices, &choices) != nil || len(choices) == 0 {
		return nil, "", 0, false
	}
	c := choices[0]
	if fr, has := c["finish_reason"]; has && string(fr) != "null" {
		return nil, "", 0, false // terminal chunk: passthrough (after a flush)
	}
	if idx, has := c["index"]; has {
		_ = json.Unmarshal(idx, &choiceIdx)
	}
	rawDelta, has := c["delta"]
	if !has {
		return nil, "", 0, false
	}
	var delta map[string]json.RawMessage
	if json.Unmarshal(rawDelta, &delta) != nil {
		return nil, "", 0, false
	}
	rawContent, has := delta["content"]
	if !has {
		return nil, "", 0, false
	}
	if json.Unmarshal(rawContent, &content) != nil || content == "" {
		return nil, "", 0, false
	}
	// tool_calls alongside content would be dropped by the synthetic-event
	// collapse; treat such chunks as passthrough instead (rare, and the
	// request plane already governed what the tools see).
	if _, has := delta["tool_calls"]; has {
		return nil, "", 0, false
	}
	return chunk, content, choiceIdx, true
}
