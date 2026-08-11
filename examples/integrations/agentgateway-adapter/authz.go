// Copyright 2026 AxonFlow contributors
// SPDX-License-Identifier: BUSL-1.1

package adapter

import (
	"context"
	"fmt"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
)

// AuthzServer implements the Envoy ext_authz v3 Authorization service.
//
// It maps every gateway request → PDP verdict → CheckResponse:
//
//	allow          → Ok  (optionally stamp x-axonflow-{trace,decision}-id)
//	deny           → Denied (HTTP 403 + structured JSON body)
//	needs_approval → Denied (HTTP 403 — treated as deny for now)
//	PDP unreachable → fail-mode: open → Ok; closed → Denied 503
//	PDP 4xx         → Denied 502 (never fail-open eligible)
//	unknown verdict → fail-mode logic
//
// The response-plane ext_proc seam and request-phase redact_pii obligation
// FULFILMENT (rewriting the body via /api/v1/mcp/check-input) are out of
// scope for the ext_authz-only hook — ext_authz can add/remove headers but
// cannot rewrite the request body. If a decide response carries a
// request-phase redact_pii obligation, the adapter fails closed rather than
// forward unredacted content (contract discipline from ADR-056).
type AuthzServer struct {
	authv3.UnimplementedAuthorizationServer
	cfg Config
	pdp *PDPClient
}

func NewAuthzServer(cfg Config, pdp *PDPClient) *AuthzServer {
	return &AuthzServer{cfg: cfg, pdp: pdp}
}

// Check is the single ext_authz RPC. Returning an *authv3.CheckResponse with
// an rpcstatus.Status of OK signals allow; any other code signals deny.
func (s *AuthzServer) Check(ctx context.Context, req *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	http := req.GetAttributes().GetRequest().GetHttp()
	headers := http.GetHeaders()

	authHeader := headers["authorization"]
	traceparent := headers["traceparent"]

	// The body only arrives on ext_authz if agentgateway's includeRequestBody
	// is set. When present, it may be truncated to maxRequestBytes. That is
	// fine for policy inspection here; we do not rewrite it.
	body := http.GetBody()
	if len(body) > s.cfg.MaxBodyBytes && s.cfg.MaxBodyBytes > 0 {
		body = body[:s.cfg.MaxBodyBytes]
	}

	decideReq := DecideRequest{
		Stage: s.cfg.StageOr("llm"),
		CallerIdentity: CallerIdentity{
			GatewayID: s.cfg.GatewayID,
			OrgID:     s.cfg.OrgID,
			TenantID:  s.cfg.TenantID,
		},
		Target: Target{
			Type: s.cfg.StageOr("llm"),
		},
		Query:     body,
		UserToken: stripBearer(authHeader),
	}

	resp, err := s.pdp.Decide(ctx, decideReq, traceparent)
	if err != nil {
		if IsRejected(err) {
			return denied(codes.PermissionDenied, 502,
				fmt.Sprintf("PDP rejected the decide request: %v", err), "", ""), nil
		}
		// Transport / 5xx: apply fail-mode.
		if s.cfg.FailOpen() {
			return allowed("", ""), nil
		}
		return denied(codes.Unavailable, 503,
			"AxonFlow PDP unreachable and fail-mode is closed", "", ""), nil
	}

	// A request-phase redact_pii obligation cannot be discharged via
	// ext_authz (body rewrites need ext_proc). Fail closed rather than
	// forward unredacted content.
	if hasRequestRedaction(resp.Obligations) {
		return denied(codes.PermissionDenied, 403,
			"request-phase redact_pii obligation cannot be discharged via ext_authz — enable ext_proc",
			resp.DecisionID, resp.TraceID), nil
	}

	switch resp.Verdict {
	case "allow":
		return allowed(resp.DecisionID, resp.TraceID), nil
	case "deny", "needs_approval":
		return denied(codes.PermissionDenied, 403,
			fmt.Sprintf("Request blocked by AxonFlow policy (verdict: %s)", resp.Verdict),
			resp.DecisionID, resp.TraceID), nil
	default:
		if s.cfg.FailOpen() {
			return allowed(resp.DecisionID, resp.TraceID), nil
		}
		return denied(codes.PermissionDenied, 403,
			fmt.Sprintf("unexpected verdict: %s", resp.Verdict),
			resp.DecisionID, resp.TraceID), nil
	}
}

func hasRequestRedaction(obs []Obligation) bool {
	for _, o := range obs {
		if o.Type == "redact_pii" && o.Fulfillment != nil && o.Fulfillment.Phase == "request" {
			return true
		}
	}
	return false
}

// allowed builds an OK CheckResponse. When decision + trace IDs are known,
// they are stamped on the upstream request as trust-boundary correlation
// headers (docs.getaxonflow.com convention).
func allowed(decisionID, traceID string) *authv3.CheckResponse {
	var addHeaders []*corev3.HeaderValueOption
	if decisionID != "" {
		addHeaders = append(addHeaders, header("x-axonflow-decision-id", decisionID))
	}
	if traceID != "" {
		addHeaders = append(addHeaders, header("x-axonflow-trace-id", traceID))
	}
	return &authv3.CheckResponse{
		Status: &rpcstatus.Status{Code: int32(codes.OK)},
		HttpResponse: &authv3.CheckResponse_OkResponse{
			OkResponse: &authv3.OkHttpResponse{
				Headers: addHeaders,
			},
		},
	}
}

// denied builds a structured JSON deny response with the given HTTP status.
func denied(grpcCode codes.Code, httpStatus int, message, decisionID, traceID string) *authv3.CheckResponse {
	body := fmt.Sprintf(
		`{"error":{"message":%q,"type":"policy_violation","decision_id":%q,"trace_id":%q}}`,
		message, decisionID, traceID,
	)
	var respHeaders []*corev3.HeaderValueOption
	respHeaders = append(respHeaders, header("content-type", "application/json"))
	if decisionID != "" {
		respHeaders = append(respHeaders, header("x-axonflow-decision-id", decisionID))
	}
	if traceID != "" {
		respHeaders = append(respHeaders, header("x-axonflow-trace-id", traceID))
	}
	return &authv3.CheckResponse{
		Status: &rpcstatus.Status{Code: int32(grpcCode), Message: message},
		HttpResponse: &authv3.CheckResponse_DeniedResponse{
			DeniedResponse: &authv3.DeniedHttpResponse{
				Status:  &typev3.HttpStatus{Code: typev3.StatusCode(httpStatus)},
				Headers: respHeaders,
				Body:    body,
			},
		},
	}
}

func header(name, value string) *corev3.HeaderValueOption {
	return &corev3.HeaderValueOption{
		Header: &corev3.HeaderValue{Key: name, Value: value},
	}
}

// stripBearer returns the token from an "Authorization: Bearer <jwt>" header.
func stripBearer(auth string) string {
	const p = "Bearer "
	if strings.HasPrefix(auth, p) {
		return strings.TrimSpace(auth[len(p):])
	}
	return ""
}
