// Copyright 2026 AxonFlow
// SPDX-License-Identifier: BUSL-1.1
package adapter

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// stubGovern masks SECRET and blocks on BLOCKME, recording segment calls.
func stubGovern(calls *[]string) GovernTextFunc {
	return func(text string) (string, bool, string) {
		*calls = append(*calls, text)
		if strings.Contains(text, "BLOCKME") {
			return "", true, "blocked by policy"
		}
		return strings.ReplaceAll(text, "SECRET", "****"), false, ""
	}
}

func chunkEvent(content string) string {
	b, _ := json.Marshal(content)
	return `data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o",` +
		`"choices":[{"index":0,"delta":{"content":` + string(b) + `},"finish_reason":null}]}` + "\n\n"
}

const roleEvent = `data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n"
const finishEvent = `data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
const doneEvent = "data: [DONE]\n\n"

// collectContent reassembles delta.content from the governor's output stream
// and returns the ordered list of raw data payloads too.
func collectContent(t *testing.T, out []byte) (string, []string) {
	t.Helper()
	var content strings.Builder
	var payloads []string
	for _, ev := range parseSSE(out) {
		data, ok := ev.data()
		if !ok {
			continue
		}
		payloads = append(payloads, data)
		if strings.TrimSpace(data) == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) == nil && len(chunk.Choices) > 0 {
			content.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	return content.String(), payloads
}

func TestStream_ShortAnswerMaskedOnFinish(t *testing.T) {
	var calls []string
	g := NewLLMStreamGovernor(stubGovern(&calls))
	var out []byte
	out = append(out, g.Feed([]byte(roleEvent))...)
	out = append(out, g.Feed([]byte(chunkEvent("the value is SEC")))...)
	out = append(out, g.Feed([]byte(chunkEvent("RET indeed")))...)
	// nothing governed yet: short content is held back until a flush point
	if c, _ := collectContent(t, out); strings.Contains(c, "SECRET") {
		t.Fatal("REDACTION LEAK before flush")
	}
	out = append(out, g.Feed([]byte(finishEvent+doneEvent))...)
	out = append(out, g.Close()...)

	content, payloads := collectContent(t, out)
	if strings.Contains(content, "SECRET") {
		t.Fatalf("REDACTION LEAK: %q", content)
	}
	if !strings.Contains(content, "the value is **** indeed") {
		t.Fatalf("masked content wrong: %q", content)
	}
	if len(calls) != 1 {
		t.Fatalf("govern calls=%d, want 1", len(calls))
	}
	// order: role prelude, masked content, finish, [DONE]
	if len(payloads) != 4 || !strings.Contains(payloads[0], `"role":"assistant"`) ||
		!strings.Contains(payloads[2], `"finish_reason":"stop"`) || strings.TrimSpace(payloads[3]) != "[DONE]" {
		t.Fatalf("event order disturbed: %v", payloads)
	}
}

func TestStream_SegmentFlushKeepsTailAndMasksStraddle(t *testing.T) {
	var calls []string
	g := NewLLMStreamGovernor(stubGovern(&calls))
	g.segmentBytes, g.tailBytes = 64, 16

	var out []byte
	// push > segment+tail (64+16) of padding, then a marker split across two
	// chunks LANDING INSIDE THE TAIL window at first flush.
	out = append(out, g.Feed([]byte(chunkEvent(strings.Repeat("x", 80)+" SEC")))...)
	if len(out) == 0 {
		t.Fatal("expected a governed segment to be released mid-stream")
	}
	if c, _ := collectContent(t, out); strings.Contains(c, "SEC") && strings.Contains(c, "RET") {
		t.Fatal("straddling marker escaped in the first segment")
	}
	out2 := g.Feed([]byte(chunkEvent("RET and more text after")))
	fin := g.Feed([]byte(finishEvent + doneEvent))
	all := append(append(append([]byte{}, out...), out2...), fin...)
	all = append(all, g.Close()...)

	content, _ := collectContent(t, all)
	if strings.Contains(content, "SECRET") {
		t.Fatalf("REDACTION LEAK across segment boundary: %q", content)
	}
	if !strings.Contains(content, "****") {
		t.Fatalf("straddling marker not masked: %q", content)
	}
	if !strings.HasPrefix(content, strings.Repeat("x", 64)) {
		t.Fatalf("early text not preserved: %q", content[:80])
	}
	if len(calls) < 2 {
		t.Fatalf("govern calls=%d, want >=2 (segment + final flush)", len(calls))
	}
}

func TestStream_BlockWithholdsRemainder(t *testing.T) {
	var calls []string
	g := NewLLMStreamGovernor(stubGovern(&calls))
	g.segmentBytes, g.tailBytes = 32, 8

	var out []byte
	out = append(out, g.Feed([]byte(chunkEvent("something BLOCKME terrible "+strings.Repeat("y", 64))))...)
	out = append(out, g.Feed([]byte(chunkEvent("this text must never reach the client")))...)
	out = append(out, g.Feed([]byte(finishEvent+doneEvent))...)
	out = append(out, g.Close()...)

	content, payloads := collectContent(t, out)
	if strings.Contains(content, "BLOCKME") || strings.Contains(content, "never reach the client") {
		t.Fatalf("blocked content leaked: %q", content)
	}
	if !strings.Contains(content, "withheld by AxonFlow policy") {
		t.Fatalf("no policy notice in stream: %q", content)
	}
	last := strings.TrimSpace(payloads[len(payloads)-1])
	if last != "[DONE]" {
		t.Fatalf("stream did not terminate cleanly: %q", last)
	}
}

func TestStream_UTF8NeverSplitAcrossEvents(t *testing.T) {
	var calls []string
	g := NewLLMStreamGovernor(stubGovern(&calls))
	g.segmentBytes, g.tailBytes = 24, 8

	rupiah := strings.Repeat("₹", 30) // 3-byte runes; cut points land mid-rune
	var out []byte
	out = append(out, g.Feed([]byte(chunkEvent(rupiah)))...)
	out = append(out, g.Feed([]byte(finishEvent+doneEvent))...)
	out = append(out, g.Close()...)

	content, _ := collectContent(t, out)
	if content != rupiah {
		t.Fatalf("multibyte content corrupted: got %d bytes, want %d", len(content), len(rupiah))
	}
	if strings.ContainsRune(content, '�') {
		t.Fatal("replacement character in output: rune split across events")
	}
}

func TestStream_CRLFAndKeepaliveTolerated(t *testing.T) {
	var calls []string
	g := NewLLMStreamGovernor(stubGovern(&calls))
	var out []byte
	crlfEvent := strings.ReplaceAll(chunkEvent("crlf SECRET framed"), "\n", "\r\n")
	out = append(out, g.Feed([]byte(": keepalive\n\n"))...)
	out = append(out, g.Feed([]byte(crlfEvent))...)
	out = append(out, g.Feed([]byte(doneEvent))...)
	out = append(out, g.Close()...)
	content, _ := collectContent(t, out)
	if strings.Contains(content, "SECRET") {
		t.Fatalf("REDACTION LEAK with CRLF framing: %q", content)
	}
	if !strings.Contains(content, "crlf **** framed") {
		t.Fatalf("CRLF event content wrong: %q", content)
	}
}

func TestStream_FragmentedEventsReassembled(t *testing.T) {
	var calls []string
	g := NewLLMStreamGovernor(stubGovern(&calls))
	full := chunkEvent("split SECRET across tcp chunks") + finishEvent + doneEvent
	var out []byte
	for i := 0; i < len(full); i += 7 { // feed in 7-byte slivers
		end := i + 7
		if end > len(full) {
			end = len(full)
		}
		out = append(out, g.Feed([]byte(full[i:end]))...)
	}
	out = append(out, g.Close()...)
	content, _ := collectContent(t, out)
	if strings.Contains(content, "SECRET") {
		t.Fatalf("REDACTION LEAK with fragmented events: %q", content)
	}
	if !strings.Contains(content, "split **** across tcp chunks") {
		t.Fatalf("fragmented content wrong: %q", content)
	}
}

func TestStream_MultiChoiceInterleaveNotCollapsed(t *testing.T) {
	var calls []string
	g := NewLLMStreamGovernor(stubGovern(&calls))
	ev := func(idx int, content string) string {
		return fmt.Sprintf(`data: {"object":"chat.completion.chunk","choices":[{"index":%d,"delta":{"content":%q},"finish_reason":null}]}`+"\n\n", idx, content)
	}
	var out []byte
	out = append(out, g.Feed([]byte(ev(0, "choice zero SECRET ")))...)
	out = append(out, g.Feed([]byte(ev(1, "choice one text")))...)
	out = append(out, g.Feed([]byte(doneEvent))...)
	out = append(out, g.Close()...)

	_, payloads := collectContent(t, out)
	// choice 0's masked text must be attributed to index 0, choice 1's to index 1
	sawZero, sawOne := false, false
	for _, p := range payloads {
		var chunk struct {
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(p), &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		c := chunk.Choices[0]
		if c.Index == 0 && strings.Contains(c.Delta.Content, "choice zero ****") {
			sawZero = true
		}
		if c.Index == 1 && strings.Contains(c.Delta.Content, "choice one text") {
			sawOne = true
		}
		if c.Index == 0 && strings.Contains(c.Delta.Content, "choice one") {
			t.Fatalf("choice 1 text collapsed into choice 0: %s", p)
		}
	}
	if !sawZero || !sawOne {
		t.Fatalf("per-choice attribution lost: %v", payloads)
	}
}
