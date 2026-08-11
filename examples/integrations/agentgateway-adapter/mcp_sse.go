// Copyright 2026 AxonFlow
// SPDX-License-Identifier: BUSL-1.1
//
// SSE-framed MCP response support for the ExtMcp shim. agentgateway (v1.3.x)
// answers POST /mcp with text/event-stream frames ("data: <json-rpc>\n\n"),
// not a bare JSON document, so the response-plane governor must unwrap SSE
// events, govern each data payload, and re-frame the stream. Without this the
// JSON parse fails and every SSE-framed result would pass ungoverned.
package adapter

import (
	"bytes"
	"context"
	"strings"
)

// looksLikeSSE reports whether body is SSE-framed rather than a bare JSON
// document: the first non-blank line starts with an SSE field name or a
// comment, never with '{' or '['.
func looksLikeSSE(body []byte) bool {
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		for _, p := range []string{"data:", "event:", "id:", "retry:", ":"} {
			if bytes.HasPrefix(line, []byte(p)) {
				return true
			}
		}
		return false
	}
	return false
}

// sseEvent is one parsed event: its original lines (line endings stripped),
// kept verbatim so untouched events re-serialize byte-faithfully.
type sseEvent struct {
	lines []string
}

// data joins the event's data lines per the SSE spec (multiple data lines
// concatenate with \n). ok is false when the event carries no data field.
func (e *sseEvent) data() (string, bool) {
	var parts []string
	for _, l := range e.lines {
		if strings.HasPrefix(l, "data:") {
			parts = append(parts, strings.TrimPrefix(strings.TrimPrefix(l, "data:"), " "))
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n"), true
}

// withData returns the event's lines with the data field replaced by a single
// "data: <payload>" line at the position of the first original data line;
// id/event/comment lines are preserved in order.
func (e *sseEvent) withData(payload string) []string {
	out := make([]string, 0, len(e.lines)+1)
	placed := false
	for _, l := range e.lines {
		if strings.HasPrefix(l, "data:") {
			if !placed {
				out = append(out, "data: "+payload)
				placed = true
			}
			continue
		}
		out = append(out, l)
	}
	if !placed {
		out = append(out, "data: "+payload)
	}
	return out
}

// parseSSE splits an SSE body into events on blank lines. Tolerates \r\n and
// a missing trailing blank line.
func parseSSE(body []byte) []sseEvent {
	var events []sseEvent
	var cur []string
	for _, raw := range bytes.Split(body, []byte("\n")) {
		line := strings.TrimRight(string(raw), "\r")
		if line == "" {
			if len(cur) > 0 {
				events = append(events, sseEvent{lines: cur})
				cur = nil
			}
			continue
		}
		cur = append(cur, line)
	}
	if len(cur) > 0 {
		events = append(events, sseEvent{lines: cur})
	}
	return events
}

// governSSEResponse governs each SSE event's data payload as a JSON-RPC
// message via the plain-JSON response governor. Any blocked payload blocks the
// whole response (the ext_proc layer turns that into an immediate 403);
// redacted payloads are re-framed in place; everything else passes verbatim.
func governSSEResponse(ctx context.Context, mcp *MCPClient, tool string, body []byte) BodyDecision {
	events := parseSSE(body)
	if len(events) == 0 {
		return BodyDecision{Action: ActionPass}
	}
	changed := false
	lastDecision := ""
	out := make([][]string, 0, len(events))
	for i := range events {
		payload, ok := events[i].data()
		if !ok {
			out = append(out, events[i].lines)
			continue
		}
		d := governJSONResponse(ctx, mcp, tool, []byte(payload))
		if d.DecisionID != "" {
			lastDecision = d.DecisionID
		}
		switch d.Action {
		case ActionBlock:
			return d
		case ActionReplace:
			// json.Marshal output contains no raw newlines, so the payload
			// always fits a single data line.
			out = append(out, events[i].withData(string(d.NewBody)))
			changed = true
		default:
			out = append(out, events[i].lines)
		}
	}
	if !changed {
		return BodyDecision{Action: ActionPass, DecisionID: lastDecision}
	}
	var b strings.Builder
	for _, ev := range out {
		for _, l := range ev {
			b.WriteString(l)
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	return BodyDecision{Action: ActionReplace, NewBody: []byte(b.String()), DecisionID: lastDecision}
}
