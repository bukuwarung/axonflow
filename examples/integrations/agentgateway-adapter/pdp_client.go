// Copyright 2026 AxonFlow contributors
// SPDX-License-Identifier: BUSL-1.1

package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DecideRequest and DecideResponse mirror POST /api/v1/decide on the AxonFlow
// agent. The wire shapes are pinned by pep_contract_test.go in the platform
// tree; the smaller struct set here is the same subset the community HTTP
// reference adapter uses (examples/integrations/decision-mode-adapter).
type DecideRequest struct {
	Stage          string         `json:"stage"`
	CallerIdentity CallerIdentity `json:"caller_identity"`
	Target         Target         `json:"target"`
	Query          string         `json:"query"`
	UserToken      string         `json:"user_token,omitempty"`
	Context        map[string]any `json:"context,omitempty"`
}

type CallerIdentity struct {
	GatewayID string `json:"gateway_id,omitempty"`
	OrgID     string `json:"org_id,omitempty"`
	TenantID  string `json:"tenant_id,omitempty"`
}

type Target struct {
	Type     string `json:"type,omitempty"`
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
}

type DecideResponse struct {
	Verdict           string       `json:"verdict"`
	DecisionID        string       `json:"decision_id"`
	TraceID           string       `json:"trace_id"`
	Reasons           []string     `json:"reasons,omitempty"`
	Obligations       []Obligation `json:"obligations"`
	EvaluatedPolicies []string     `json:"evaluated_policies"`
	Stage             string       `json:"stage,omitempty"`
	ExpiresAt         time.Time    `json:"expires_at"`
	Error             string       `json:"error,omitempty"`
}

type Obligation struct {
	Type        string                 `json:"type"`
	Detail      string                 `json:"detail,omitempty"`
	Fulfillment *ObligationFulfillment `json:"fulfillment,omitempty"`
}

type ObligationFulfillment struct {
	Endpoint     string   `json:"endpoint"`
	Method       string   `json:"method"`
	Phase        string   `json:"phase"`
	ContentTypes []string `json:"content_types,omitempty"`
}

// ErrDecisionRejected wraps a PDP 4xx. These are never fail-open eligible —
// they indicate a real problem the caller must fix (bad credentials, rate
// limit, identity mismatch), not transient degradation.
type ErrDecisionRejected struct {
	StatusCode int
	Body       string
}

func (e *ErrDecisionRejected) Error() string {
	return fmt.Sprintf("PDP rejected decide request (%d): %s", e.StatusCode, e.Body)
}

// PDPClient is a thin HTTP client over the AxonFlow Decision API.
type PDPClient struct {
	cfg  Config
	http *http.Client
}

func NewPDPClient(cfg Config) *PDPClient {
	return &PDPClient{
		cfg:  cfg,
		http: &http.Client{Timeout: cfg.RequestTimeout},
	}
}

// Decide asks the PDP for a verdict on req. It returns (*ErrDecisionRejected)
// for 4xx responses and a wrapped error for transport / 5xx failures.
func (c *PDPClient) Decide(ctx context.Context, req DecideRequest, traceparent string) (*DecideResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal decide request: %w", err)
	}
	url := strings.TrimRight(c.cfg.AxonFlowEndpoint, "/") + "/api/v1/decide"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build decide request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if traceparent != "" {
		httpReq.Header.Set("traceparent", traceparent)
	}
	if c.cfg.OrgID != "" && c.cfg.LicenseKey != "" {
		httpReq.SetBasicAuth(c.cfg.OrgID, c.cfg.LicenseKey)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("decide call failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read decide response: %w", err)
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("PDP decide returned %d: %s", resp.StatusCode, string(respBody))
	}
	if resp.StatusCode >= 400 {
		return nil, &ErrDecisionRejected{StatusCode: resp.StatusCode, Body: string(respBody)}
	}
	var out DecideResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode decide response: %w", err)
	}
	return &out, nil
}

// IsRejected reports whether err is a PDP 4xx.
func IsRejected(err error) bool {
	var r *ErrDecisionRejected
	return errors.As(err, &r)
}
