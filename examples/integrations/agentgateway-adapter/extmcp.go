// Copyright 2026 AxonFlow
// SPDX-License-Identifier: BUSL-1.1
//
// ExtMcp shim (AID-212 follow-up): an Envoy ext_proc v3 processor that governs
// MCP tool args (request body) and results (response body) through the AxonFlow
// agent's check-input / check-output planes. This is the piece the ext_authz
// adapter cannot do — ext_authz allows/denies but cannot rewrite bodies, so it
// fails closed on redaction; this processor performs the redaction.
//
// Configure agentgateway/Envoy to send BUFFERED request and response bodies to
// this processor (see the handoff doc).
package adapter

import (
	"io"
	"log"

	ext_proc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
)

// ExtMcpServer implements envoy.service.ext_proc.v3.ExternalProcessor.
type ExtMcpServer struct {
	ext_proc.UnimplementedExternalProcessorServer
	cfg      Config
	mcp      *MCPClient
	failOpen bool
}

// NewExtMcpServer wires the processor to the MCP PDP client.
func NewExtMcpServer(cfg Config, mcp *MCPClient) *ExtMcpServer {
	return &ExtMcpServer{cfg: cfg, mcp: mcp, failOpen: cfg.FailOpen()}
}

// Process is the ext_proc bidirectional stream. One message per phase; we act on
// the request/response bodies and pass headers through.
func (s *ExtMcpServer) Process(stream ext_proc.ExternalProcessor_ProcessServer) error {
	ctx := stream.Context()
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
			resp = s.bodyResponse(GovernRequestBody(ctx, s.mcp, v.RequestBody.GetBody()), true)
		case *ext_proc.ProcessingRequest_ResponseHeaders:
			resp = headersContinue(false)
		case *ext_proc.ProcessingRequest_ResponseBody:
			// tool name is not re-derivable here without request correlation;
			// govern the result content generically (tool="").
			resp = s.bodyResponse(GovernResponseBody(ctx, s.mcp, "", v.ResponseBody.GetBody()), false)
		default:
			resp = &ext_proc.ProcessingResponse{}
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

func headersContinue(isRequest bool) *ext_proc.ProcessingResponse {
	hr := &ext_proc.HeadersResponse{Response: &ext_proc.CommonResponse{Status: ext_proc.CommonResponse_CONTINUE}}
	if isRequest {
		return &ext_proc.ProcessingResponse{Response: &ext_proc.ProcessingResponse_RequestHeaders{RequestHeaders: hr}}
	}
	return &ext_proc.ProcessingResponse{Response: &ext_proc.ProcessingResponse_ResponseHeaders{ResponseHeaders: hr}}
}

// bodyResponse turns a BodyDecision into the ext_proc reply: pass, replace, or
// an immediate 403 for a block.
func (s *ExtMcpServer) bodyResponse(d BodyDecision, isRequest bool) *ext_proc.ProcessingResponse {
	switch d.Action {
	case ActionBlock:
		log.Printf("mcp-shim: BLOCK (%s) decision=%s: %s", phase(isRequest), d.DecisionID, d.Reason)
		return &ext_proc.ProcessingResponse{
			Response: &ext_proc.ProcessingResponse_ImmediateResponse{
				ImmediateResponse: &ext_proc.ImmediateResponse{
					Status:  &typev3.HttpStatus{Code: typev3.StatusCode_Forbidden},
					Body:    []byte(denyBody(d)),
					Details: "axonflow-mcp-policy",
				},
			},
		}
	case ActionReplace:
		log.Printf("mcp-shim: REDACT (%s) decision=%s", phase(isRequest), d.DecisionID)
		cr := &ext_proc.CommonResponse{
			Status:       ext_proc.CommonResponse_CONTINUE_AND_REPLACE,
			BodyMutation: &ext_proc.BodyMutation{Mutation: &ext_proc.BodyMutation_Body{Body: d.NewBody}},
		}
		return bodyReply(cr, isRequest)
	default:
		return bodyReply(&ext_proc.CommonResponse{Status: ext_proc.CommonResponse_CONTINUE}, isRequest)
	}
}

func bodyReply(cr *ext_proc.CommonResponse, isRequest bool) *ext_proc.ProcessingResponse {
	br := &ext_proc.BodyResponse{Response: cr}
	if isRequest {
		return &ext_proc.ProcessingResponse{Response: &ext_proc.ProcessingResponse_RequestBody{RequestBody: br}}
	}
	return &ext_proc.ProcessingResponse{Response: &ext_proc.ProcessingResponse_ResponseBody{ResponseBody: br}}
}

func phase(isRequest bool) string {
	if isRequest {
		return "request"
	}
	return "response"
}

func denyBody(d BodyDecision) string {
	return `{"error":{"type":"policy_violation","message":"` + jsonEscape(d.Reason) + `","decision_id":"` + jsonEscape(d.DecisionID) + `"}}`
}

func jsonEscape(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case '"', '\\':
			out = append(out, '\\', r)
		case '\n', '\r', '\t':
			out = append(out, ' ')
		default:
			out = append(out, r)
		}
	}
	return string(out)
}
