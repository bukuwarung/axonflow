// Copyright 2026 AxonFlow
// SPDX-License-Identifier: BUSL-1.1
//
// LLM ext_proc shim (AID-100 Fix B): an Envoy ext_proc v3 processor that
// restores body redaction on the LLM plane — the seam ext_authz cannot
// provide. Chat-completions REQUESTS (buffered) are governed via check-input
// before the provider sees them; RESPONSES are governed via check-output —
// non-streaming bodies in one pass, streaming SSE through LLMStreamGovernor
// with FULL_DUPLEX_STREAMED chunks, so Cortex's streaming UX survives.
//
// Gateway config pairing (LLM listener, conditional on POST):
//
//	extProc:
//	  conditional:
//	    - condition: 'request.method == "POST"'
//	      host: axonflow-llm-shim:9092
//	      failureMode: failClosed
//	      processingOptions:
//	        requestHeaderMode: send
//	        responseHeaderMode: send
//	        requestBodyMode: buffered
//	        responseBodyMode: fullDuplexStreamed
//
// requestBodyMode buffered ⇒ the whole request arrives in one message and is
// replaced with Mutation::Body + an exact content-length (the gateway
// validates the header against the mutated body length — fork d9f5262f).
// responseBodyMode fullDuplexStreamed ⇒ response chunks stream through and
// are re-emitted as StreamedResponse mutations, decoupled from input framing;
// the gateway strips content-length on this path, so held-back text can be
// released later without a length mismatch.
package adapter

import (
	"context"
	"io"
	"log"

	ext_proc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
)

// ExtLLMServer implements envoy.service.ext_proc.v3.ExternalProcessor for the
// LLM (chat-completions) plane.
type ExtLLMServer struct {
	ext_proc.UnimplementedExternalProcessorServer
	cfg Config
	mcp *MCPClient
}

// NewExtLLMServer wires the processor to the MCP PDP client (check-input /
// check-output are generic text planes; connector labels distinguish "llm").
func NewExtLLMServer(cfg Config, mcp *MCPClient) *ExtLLMServer {
	return &ExtLLMServer{cfg: cfg, mcp: mcp}
}

// llmExchange is the per-HTTP-exchange state of one ext_proc stream.
type llmExchange struct {
	reqBuf   []byte             // buffered-mode request accumulation
	model    string             // from the request, labels PDP calls + audit
	respSSE  bool               // response content-type is text/event-stream
	respBuf  []byte             // non-SSE response accumulation
	gov      *LLMStreamGovernor // SSE response governor
	blockedR bool               // request was blocked; ignore any response
}

// Process is the ext_proc bidirectional stream: one HTTP exchange per stream.
func (s *ExtLLMServer) Process(stream ext_proc.ExternalProcessor_ProcessServer) error {
	ctx := stream.Context()
	ex := &llmExchange{}
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		var resp *ext_proc.ProcessingResponse
		switch v := req.Request.(type) {
		case *ext_proc.ProcessingRequest_RequestHeaders:
			resp = headersContinue(true)
		case *ext_proc.ProcessingRequest_RequestBody:
			resp = s.onRequestBody(ctx, ex, v.RequestBody)
		case *ext_proc.ProcessingRequest_ResponseHeaders:
			ex.respSSE = headerContains(v.ResponseHeaders, "content-type", "text/event-stream")
			resp = headersContinue(false)
		case *ext_proc.ProcessingRequest_ResponseBody:
			resp = s.onResponseBody(ctx, ex, v.ResponseBody)
		case *ext_proc.ProcessingRequest_RequestTrailers:
			resp = &ext_proc.ProcessingResponse{
				Response: &ext_proc.ProcessingResponse_RequestTrailers{RequestTrailers: &ext_proc.TrailersResponse{}},
			}
		case *ext_proc.ProcessingRequest_ResponseTrailers:
			// FULL_DUPLEX forces trailer mode Send. If pending text survived to
			// the trailers (no eos flag on the last body chunk), release it
			// before answering the trailers so nothing is truncated.
			if ex.gov != nil {
				if tail := ex.gov.Close(); len(tail) > 0 {
					if err := stream.Send(streamedBodyChunk(tail, false, true)); err != nil {
						return err
					}
				}
				ex.gov = nil
			}
			resp = &ext_proc.ProcessingResponse{
				Response: &ext_proc.ProcessingResponse_ResponseTrailers{ResponseTrailers: &ext_proc.TrailersResponse{}},
			}
		default:
			resp = &ext_proc.ProcessingResponse{}
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

// onRequestBody governs the buffered chat-completions request.
func (s *ExtLLMServer) onRequestBody(ctx context.Context, ex *llmExchange, body *ext_proc.HttpBody) *ext_proc.ProcessingResponse {
	ex.reqBuf = append(ex.reqBuf, body.GetBody()...)
	if !body.GetEndOfStream() {
		// Unreachable under requestBodyMode: buffered (the gateway sends ONE
		// full-body message). Any reply to a request-body message is terminal
		// for the request phase, so there is no reply that both keeps waiting
		// AND stays governed — fail closed, loudly, so a mode misconfiguration
		// (e.g. request side flipped to fullDuplexStreamed) cannot silently
		// forward ungoverned prompts.
		ex.blockedR = true
		log.Printf("llm-shim: BLOCK (request) unexpected partial buffered body (%dB, eos=false) — check requestBodyMode", len(ex.reqBuf))
		return immediate403(`{"error":{"type":"policy_violation","message":"request arrived unbuffered; the governance seam requires requestBodyMode: buffered"}}`)
	}
	full := ex.reqBuf
	ex.reqBuf = nil
	if s.cfg.MaxBodyBytes > 0 && len(full) > s.cfg.MaxBodyBytes {
		// larger than the PDP ceiling: refusing beats forwarding ungoverned.
		ex.blockedR = true
		log.Printf("llm-shim: BLOCK (request) body %dB exceeds ceiling %dB", len(full), s.cfg.MaxBodyBytes)
		return immediate403(`{"error":{"type":"policy_violation","message":"request body exceeds the governable size ceiling"}}`)
	}
	if cr, ok := parseChatRequest(full); ok {
		ex.model = cr.model
	}
	d := GovernLLMRequestBody(ctx, s.mcp, full)
	switch d.Action {
	case ActionBlock:
		ex.blockedR = true
		log.Printf("llm-shim: BLOCK (request) decision=%s: %s", d.DecisionID, d.Reason)
		return immediate403(denyBody(d))
	case ActionReplace:
		log.Printf("llm-shim: REDACT (request) decision=%s", d.DecisionID)
		return bodyReply(&ext_proc.CommonResponse{
			Status:         ext_proc.CommonResponse_CONTINUE_AND_REPLACE,
			BodyMutation:   &ext_proc.BodyMutation{Mutation: &ext_proc.BodyMutation_Body{Body: d.NewBody}},
			HeaderMutation: contentLengthMutation(true, len(d.NewBody)),
		}, true)
	default:
		return bodyReply(&ext_proc.CommonResponse{Status: ext_proc.CommonResponse_CONTINUE}, true)
	}
}

// onResponseBody governs full-duplex response chunks: SSE through the stream
// governor, JSON accumulated and governed at end of stream.
func (s *ExtLLMServer) onResponseBody(ctx context.Context, ex *llmExchange, body *ext_proc.HttpBody) *ext_proc.ProcessingResponse {
	eos := body.GetEndOfStream()
	if ex.blockedR {
		return streamedBodyChunk(nil, false, eos)
	}
	if ex.respSSE {
		if ex.gov == nil {
			ex.gov = NewLLMStreamGovernor(func(text string) (string, bool, string) {
				v := s.mcp.CheckOutput(ctx, ex.model, text)
				if !v.Allowed {
					log.Printf("llm-shim: BLOCK (response segment) decision=%s: %s", v.DecisionID, v.Reason)
					return "", true, v.Reason
				}
				if v.WasRedacted {
					log.Printf("llm-shim: REDACT (response segment) decision=%s", v.DecisionID)
					return v.Redacted, false, ""
				}
				return text, false, ""
			})
		}
		out := ex.gov.Feed(body.GetBody())
		if eos {
			out = append(out, ex.gov.Close()...)
			ex.gov = nil
		}
		return streamedBodyChunk(out, false, eos)
	}
	// non-SSE: hold chunks, govern once complete. Emitting nothing until eos
	// is legal in full-duplex mode (output framing is decoupled from input).
	ex.respBuf = append(ex.respBuf, body.GetBody()...)
	if !eos {
		return streamedBodyChunk(nil, false, false)
	}
	full := ex.respBuf
	ex.respBuf = nil
	d := GovernLLMResponseJSON(ctx, s.mcp, ex.model, full)
	switch d.Action {
	case ActionBlock:
		// response headers already went downstream; substituting the body is
		// the strongest containment still available on this path.
		log.Printf("llm-shim: BLOCK (response) decision=%s: %s", d.DecisionID, d.Reason)
		return streamedBodyChunk([]byte(denyBody(d)), false, true)
	case ActionReplace:
		log.Printf("llm-shim: REDACT (response) decision=%s", d.DecisionID)
		return streamedBodyChunk(d.NewBody, false, true)
	default:
		return streamedBodyChunk(full, false, true)
	}
}

// streamedBodyChunk wraps bytes as a full-duplex StreamedResponse mutation.
// asRequest selects the request-body variant (only used for the trailers-time
// flush, which is always response-side; kept explicit for symmetry).
func streamedBodyChunk(body []byte, asRequest, eos bool) *ext_proc.ProcessingResponse {
	cr := &ext_proc.CommonResponse{
		Status: ext_proc.CommonResponse_CONTINUE,
		BodyMutation: &ext_proc.BodyMutation{
			Mutation: &ext_proc.BodyMutation_StreamedResponse{
				StreamedResponse: &ext_proc.StreamedBodyResponse{Body: body, EndOfStream: eos},
			},
		},
	}
	return bodyReply(cr, asRequest)
}

func immediate403(body string) *ext_proc.ProcessingResponse {
	return &ext_proc.ProcessingResponse{
		Response: &ext_proc.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &ext_proc.ImmediateResponse{
				Status:  &typev3.HttpStatus{Code: typev3.StatusCode_Forbidden},
				Body:    []byte(body),
				Details: "axonflow-llm-policy",
			},
		},
	}
}

// headerContains reports whether a header (case-insensitive key) contains the
// given substring in its value.
func headerContains(headers *ext_proc.HttpHeaders, key, substr string) bool {
	if headers == nil {
		return false
	}
	for _, h := range headers.GetHeaders().GetHeaders() {
		if !asciiEqualFold(h.GetKey(), key) {
			continue
		}
		val := string(h.GetRawValue())
		if val == "" {
			val = h.GetValue()
		}
		if containsFold(val, substr) {
			return true
		}
	}
	return false
}

func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func containsFold(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		if asciiEqualFold(s[i:i+len(substr)], substr) {
			return true
		}
	}
	return false
}
