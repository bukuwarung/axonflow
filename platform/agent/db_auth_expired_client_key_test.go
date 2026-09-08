//go:build !enterprise

// Copyright 2026 BukuWarung (fork, AID-247)
//
// Licensed under the Business Source License 1.1 — internal use.

package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"axonflow/platform/agent/license"
)

// These tests pin the BukuWarung fork behaviour in acceptExpiredClientKey:
// an EXPIRED (past grace) but correctly signed service key remains a valid
// client credential ONLY when the deployment itself runs without a licence,
// and the client is then pinned to Community tier. Every other verdict keeps
// its vendor behaviour.

func withClientKeyValidator(t *testing.T, result *license.ValidationResult) {
	t.Helper()
	prev := validateLicenseFn
	validateLicenseFn = func(context.Context, string) (*license.ValidationResult, error) {
		return result, nil
	}
	t.Cleanup(func() { validateLicenseFn = prev })
}

// expiredServiceKeyResult mirrors what license.ValidateLicense returns for a
// correctly signed service licence whose expiry is more than seven days past:
// the payload is fully decoded, Valid is false, Error is LICENSE_EXPIRED.
func expiredServiceKeyResult() *license.ValidationResult {
	return &license.ValidationResult{
		Valid:        false,
		Error:        "LICENSE_EXPIRED",
		Tier:         license.TierEnterprise,
		DeploymentID: "bukuwarung-eval",
		OrgID:        "bukuwarung-eval",
		ServiceName:  "platform",
		ServiceType:  "backend-service",
		Permissions:  []string{"mcp:*:*", "llm:*:*"},
		ExpiresAt:    time.Now().Add(-9 * 24 * time.Hour),
		Limits:       license.GetTierLimits(license.TierEnterprise),
	}
}

func TestExpiredClientKey_AcceptedWhenDeploymentUnlicensed(t *testing.T) {
	t.Setenv("AXONFLOW_LICENSE_KEY", "")
	withClientKeyValidator(t, expiredServiceKeyResult())

	client, err := validateViaOrganizations(context.Background(), nil, "bukuwarung-eval", "AXON-payload.signature")
	if err != nil {
		t.Fatalf("expected the expired-but-signed key to authenticate, got: %v", err)
	}
	if client.LicenseTier != string(license.TierCommunity) {
		t.Fatalf("client must be pinned to Community tier, got %q", client.LicenseTier)
	}
	if client.RateLimit != 100 {
		t.Fatalf("Community client must get the default rate limit (100), got %d", client.RateLimit)
	}
	if client.OrgID != "bukuwarung-eval" || client.TenantID != "bukuwarung-eval" || client.ServiceName != "platform" {
		t.Fatalf("signed identity must be preserved, got %+v", client)
	}
	if len(client.Permissions) != 2 {
		t.Fatalf("signed permissions must be preserved, got %v", client.Permissions)
	}
}

func TestExpiredClientKey_RejectedWhenDeploymentLicensed(t *testing.T) {
	t.Setenv("AXONFLOW_LICENSE_KEY", "AXON-this-deployment-has-its-own-key.sig")
	withClientKeyValidator(t, expiredServiceKeyResult())

	_, err := validateViaOrganizations(context.Background(), nil, "bukuwarung-eval", "AXON-payload.signature")
	if err == nil || !strings.Contains(err.Error(), "license invalid or expired") {
		t.Fatalf("a licensed deployment must keep rejecting expired client keys, got: %v", err)
	}
}

func TestExpiredClientKey_OtherInvalidVerdictsStillRejected(t *testing.T) {
	t.Setenv("AXONFLOW_LICENSE_KEY", "")
	for _, code := range []string{"LICENSE_INVALID_SIGNATURE", "LICENSE_REVOKED", "SOMETHING_ELSE", ""} {
		r := expiredServiceKeyResult()
		r.Error = code
		withClientKeyValidator(t, r)
		_, err := validateViaOrganizations(context.Background(), nil, "bukuwarung-eval", "AXON-payload.signature")
		if err == nil || !strings.Contains(err.Error(), "license invalid or expired") {
			t.Fatalf("verdict %q must still be rejected, got: %v", code, err)
		}
	}
}

func TestExpiredClientKey_StillRequiresServiceIdentity(t *testing.T) {
	t.Setenv("AXONFLOW_LICENSE_KEY", "")
	r := expiredServiceKeyResult()
	r.ServiceName = ""
	withClientKeyValidator(t, r)

	_, err := validateViaOrganizations(context.Background(), nil, "bukuwarung-eval", "AXON-payload.signature")
	if err == nil || !strings.Contains(err.Error(), "service identity") {
		t.Fatalf("an expired key without service_name must still be rejected, got: %v", err)
	}
}

func TestAcceptExpiredClientKey_GuardConditions(t *testing.T) {
	t.Setenv("AXONFLOW_LICENSE_KEY", "")
	if acceptExpiredClientKey(nil) {
		t.Fatal("nil result must not be accepted")
	}
	valid := expiredServiceKeyResult()
	valid.Valid = true
	valid.Error = ""
	if acceptExpiredClientKey(valid) {
		t.Fatal("a valid result is not the expired case and must not be rewritten")
	}
	if valid.Tier != license.TierEnterprise {
		t.Fatal("a valid result's tier must be left untouched")
	}
	expired := expiredServiceKeyResult()
	if !acceptExpiredClientKey(expired) {
		t.Fatal("expired verdict on an unlicensed deployment must be accepted")
	}
	if expired.Tier != license.TierCommunity || expired.Limits.AuditRetentionDays != license.CommunityLimits.AuditRetentionDays {
		t.Fatalf("accepted result must be pinned to Community tier/limits, got tier=%s", expired.Tier)
	}
}
