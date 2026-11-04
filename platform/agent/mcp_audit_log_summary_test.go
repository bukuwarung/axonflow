// Copyright 2026 AxonFlow
// SPDX-License-Identifier: BUSL-1.1

package agent

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

var summaryStart = time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)

func TestMCPAuditLogSummary_FirstWriteIsLoggedAtOnce(t *testing.T) {
	s := newMCPAuditLogSummary(5 * time.Minute)

	got := s.record(MCPQueryAuditEntry{ConnectorName: "otel-audit", ResponseRedacted: true}, summaryStart)

	want := "[MCP Audit] Logged 1 entry (first since start; later writes are summarized every 5m0s): blocked=0, redacted=1, exfil=0, connectors: otel-audit=1"
	if got != want {
		t.Fatalf("first write:\n got %q\nwant %q", got, want)
	}
}

func TestMCPAuditLogSummary_OneLinePerInterval(t *testing.T) {
	s := newMCPAuditLogSummary(5 * time.Minute)
	if s.record(MCPQueryAuditEntry{ConnectorName: "otel-audit"}, summaryStart) == "" {
		t.Fatal("first write was not logged")
	}

	// One write a second for the rest of the interval: nothing is logged.
	for i := 1; i < 300; i++ {
		entry := MCPQueryAuditEntry{
			ConnectorName:    "otel-audit",
			RequestBlocked:   i%100 == 0, // 100, 200
			ResponseRedacted: i%10 == 0,  // 10, 20, ..., 290
		}
		if i == 150 {
			entry.ConnectorName = "postgres"
			entry.ExfilExceeded = true
		}
		if line := s.record(entry, summaryStart.Add(time.Duration(i)*time.Second)); line != "" {
			t.Fatalf("write %d inside the interval was logged: %q", i, line)
		}
	}

	// The first write once the interval has passed reports the whole window.
	got := s.record(MCPQueryAuditEntry{ConnectorName: "otel-audit"}, summaryStart.Add(5*time.Minute))
	want := "[MCP Audit] Logged 300 entries in the last 5m0s: blocked=2, redacted=29, exfil=1, connectors: otel-audit=299, postgres=1"
	if got != want {
		t.Fatalf("summary:\n got %q\nwant %q", got, want)
	}

	// The counts restart after a line, and the next line waits a full interval.
	if line := s.record(MCPQueryAuditEntry{ConnectorName: "otel-audit"}, summaryStart.Add(5*time.Minute+time.Second)); line != "" {
		t.Fatalf("write right after a line was logged: %q", line)
	}

	// After a quiet spell the next write reports the time since the last line.
	got = s.record(MCPQueryAuditEntry{ConnectorName: "otel-audit", RequestBlocked: true}, summaryStart.Add(time.Hour))
	want = "[MCP Audit] Logged 2 entries in the last 55m0s: blocked=1, redacted=0, exfil=0, connectors: otel-audit=2"
	if got != want {
		t.Fatalf("summary after a quiet spell:\n got %q\nwant %q", got, want)
	}
}

func TestMCPAuditLogSummary_BoundsConnectorNames(t *testing.T) {
	s := newMCPAuditLogSummary(time.Minute)
	s.record(MCPQueryAuditEntry{ConnectorName: "otel-audit"}, summaryStart)

	// Three times as many distinct names as the bound: the first names keep
	// their own count and the rest are counted under "other".
	for i := 0; i < 3*mcpAuditLogMaxConnectors; i++ {
		s.record(MCPQueryAuditEntry{ConnectorName: fmt.Sprintf("c%02d", i)}, summaryStart.Add(time.Second))
	}
	got := s.record(MCPQueryAuditEntry{ConnectorName: "c00"}, summaryStart.Add(time.Minute))

	counts := []string{"c00=2"}
	for i := 1; i < mcpAuditLogMaxConnectors; i++ {
		counts = append(counts, fmt.Sprintf("c%02d=1", i))
	}
	counts = append(counts, fmt.Sprintf("other=%d", 2*mcpAuditLogMaxConnectors))
	want := fmt.Sprintf("[MCP Audit] Logged %d entries in the last 1m0s: blocked=0, redacted=0, exfil=0, connectors: %s",
		3*mcpAuditLogMaxConnectors+1, strings.Join(counts, ", "))
	if got != want {
		t.Fatalf("bounded summary:\n got %q\nwant %q", got, want)
	}
}

func TestMCPAuditLogSummary_EmptyConnectorName(t *testing.T) {
	s := newMCPAuditLogSummary(time.Minute)

	got := s.record(MCPQueryAuditEntry{}, summaryStart)

	if !strings.HasSuffix(got, "connectors: (none)=1") {
		t.Fatalf("entry without a connector name: %q", got)
	}
}

func TestMCPAuditLogSummary_ConcurrentWritesAreAllCounted(t *testing.T) {
	s := newMCPAuditLogSummary(time.Hour)
	s.record(MCPQueryAuditEntry{ConnectorName: "otel-audit"}, summaryStart)

	const goroutines, writes = 8, 1000
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < writes; i++ {
				if line := s.record(MCPQueryAuditEntry{ConnectorName: "otel-audit"}, summaryStart.Add(time.Minute)); line != "" {
					t.Errorf("write inside the interval was logged: %q", line)
					return
				}
			}
		}()
	}
	wg.Wait()

	got := s.record(MCPQueryAuditEntry{ConnectorName: "otel-audit"}, summaryStart.Add(time.Hour))
	total := goroutines*writes + 1
	want := fmt.Sprintf("[MCP Audit] Logged %d entries in the last 1h0m0s: blocked=0, redacted=0, exfil=0, connectors: otel-audit=%d", total, total)
	if got != want {
		t.Fatalf("summary after concurrent writes:\n got %q\nwant %q", got, want)
	}
}

// TestLogMCPQueryAudit_SummarizesSuccessfulWrites drives the real write path:
// several successful writes inside one interval produce a single log line.
func TestLogMCPQueryAudit_SummarizesSuccessfulWrites(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create mock db: %v", err)
	}
	defer db.Close()

	auditQueue, err := NewAuditQueue(AuditModeCompliance, 100, 1, db, filepath.Join(t.TempDir(), "mcp-audit-summary.log"))
	if err != nil {
		t.Fatalf("failed to create audit queue: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = auditQueue.Shutdown(ctx)
	}()

	oldAuditManager := auditManager
	auditManager = &AuditManager{queue: auditQueue}
	defer func() { auditManager = oldAuditManager }()

	oldSummary := mcpAuditLog
	mcpAuditLog = newMCPAuditLogSummary(time.Hour)
	defer func() { mcpAuditLog = oldSummary }()

	var buf bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(oldOutput)

	// Blocked entries are written synchronously in compliance mode, so each
	// write has finished when logMCPQueryAudit returns.
	const writes = 3
	for i := 0; i < writes; i++ {
		mock.ExpectBegin()
		mock.ExpectExec(`SELECT set_config\('app.current_org_id'`).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("INSERT INTO mcp_query_audits").
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()
	}
	for i := 0; i < writes; i++ {
		logMCPQueryAudit(MCPQueryAuditEntry{
			AuditID:        fmt.Sprintf("summary-%d", i),
			TenantID:       "tenant-1",
			OrgID:          "tenant-1",
			ClientID:       "client-1",
			ConnectorName:  "otel-audit",
			Operation:      "check_input",
			RequestBlocked: true,
		})
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled mock expectations: %v", err)
	}
	if n := strings.Count(buf.String(), "[MCP Audit] Logged"); n != 1 {
		t.Fatalf("%d successful writes logged %d lines, want 1:\n%s", writes, n, buf.String())
	}
}
