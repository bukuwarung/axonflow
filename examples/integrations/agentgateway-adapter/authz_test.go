// Copyright 2026 AxonFlow contributors
// SPDX-License-Identifier: BUSL-1.1

package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/grpc/codes"
)

// stubPDP returns the given verdict for every /api/v1/decide call.
func stubPDP(t *testing.T, verdict string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/v1/decide") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(DecideResponse{
			Verdict:    verdict,
			DecisionID: "test-decision-42",
			TraceID:    "0af7651916cd43dd8448eb211c80319c",
		})
	}))
}

func newCheckRequest(body string) *authv3.CheckRequest {
	return &authv3.CheckRequest{
		Attributes: &authv3.AttributeContext{
			Request: &authv3.AttributeContext_Request{
				Http: &authv3.AttributeContext_HttpRequest{
					Method: "POST",
					Path:   "/v1/chat/completions",
					Body:   body,
					Headers: map[string]string{
						"content-type":  "application/json",
						"authorization": "Bearer test-user-jwt",
					},
				},
			},
		},
	}
}

func newAuthz(t *testing.T, endpoint, failMode string) *AuthzServer {
	t.Helper()
	cfg := Config{
		Listen:           ":0",
		AxonFlowEndpoint: endpoint,
		OrgID:            "test-org",
		TenantID:         "test-tenant",
		GatewayID:        "test-gw",
		Stage:            "llm",
		FailMode:         failMode,
		RequestTimeout:   2 * time.Second,
		MaxBodyBytes:     1024,
	}
	return NewAuthzServer(cfg, NewPDPClient(cfg))
}

func TestCheck_AllowStampsHeaders(t *testing.T) {
	pdp := stubPDP(t, "allow")
	defer pdp.Close()
	srv := newAuthz(t, pdp.URL, "closed")

	resp, err := srv.Check(context.Background(), newCheckRequest(`{"model":"gpt-4o","messages":[]}`))
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if resp.Status.Code != int32(codes.OK) {
		t.Fatalf("expected OK, got code=%d", resp.Status.Code)
	}
	ok := resp.GetOkResponse()
	if ok == nil {
		t.Fatalf("expected OkResponse, got %T", resp.HttpResponse)
	}
	sawDecision := false
	for _, h := range ok.Headers {
		if h.Header.Key == "x-axonflow-decision-id" && h.Header.Value == "test-decision-42" {
			sawDecision = true
		}
	}
	if !sawDecision {
		t.Fatalf("expected x-axonflow-decision-id header on allow, got %+v", ok.Headers)
	}
}

func TestCheck_DenyReturns403(t *testing.T) {
	pdp := stubPDP(t, "deny")
	defer pdp.Close()
	srv := newAuthz(t, pdp.URL, "closed")

	resp, err := srv.Check(context.Background(), newCheckRequest(`{"model":"gpt-4o","messages":[]}`))
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if resp.Status.Code != int32(codes.PermissionDenied) {
		t.Fatalf("expected PermissionDenied, got code=%d", resp.Status.Code)
	}
	denied := resp.GetDeniedResponse()
	if denied == nil {
		t.Fatalf("expected DeniedResponse, got %T", resp.HttpResponse)
	}
	if denied.Status.Code != 403 {
		t.Fatalf("expected HTTP 403 on deny, got %d", denied.Status.Code)
	}
	if !strings.Contains(denied.Body, "test-decision-42") {
		t.Fatalf("deny body should carry decision_id, got %q", denied.Body)
	}
}

func TestCheck_TransportErrorRespectsFailMode(t *testing.T) {
	srv := newAuthz(t, "http://127.0.0.1:1", "closed")
	resp, err := srv.Check(context.Background(), newCheckRequest(""))
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if resp.Status.Code != int32(codes.Unavailable) {
		t.Fatalf("expected Unavailable on fail-closed transport error, got code=%d", resp.Status.Code)
	}

	srv2 := newAuthz(t, "http://127.0.0.1:1", "open")
	resp2, err := srv2.Check(context.Background(), newCheckRequest(""))
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if resp2.Status.Code != int32(codes.OK) {
		t.Fatalf("expected OK on fail-open transport error, got code=%d", resp2.Status.Code)
	}
}
