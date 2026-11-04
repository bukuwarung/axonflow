// Copyright 2026 AxonFlow
// SPDX-License-Identifier: BUSL-1.1

package agent

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// mcpAuditLogInterval is how often logMCPQueryAudit reports the MCP audit
// entries the audit queue accepted.
//
// Every governed check (check-input, check-output, connector query/execute)
// records one mcp_query_audits entry, and logging each one repeated that row
// in the log stream. On a fleet whose audit plane redacts every stored
// telemetry payload through check-output, that was one to five lines a
// second, and it flooded Datadog. One summary line per interval keeps the
// signal (entries are flowing, and how many were blocked, redacted or over
// an exfiltration limit) at a fixed, small volume. Entries the queue rejects
// are still logged on every call, and the queue logs its own write failures.
const mcpAuditLogInterval = 5 * time.Minute

// Connector names come from the request (connector_type), so a summary keeps
// its own count for at most mcpAuditLogMaxConnectors names, cut to
// mcpAuditLogMaxNameLen bytes, and counts the rest together as other, instead
// of growing the map and the log line with whatever a caller sends.
const (
	mcpAuditLogMaxConnectors = 20
	mcpAuditLogMaxNameLen    = 64
)

// mcpAuditLogSummary counts accepted MCP audit entries between log lines.
type mcpAuditLogSummary struct {
	mu       sync.Mutex
	interval time.Duration
	// lastLine is zero until the first line, so the first entry after start
	// is logged at once.
	lastLine   time.Time
	total      int
	blocked    int
	redacted   int
	exfil      int
	connectors map[string]int
	// otherConnectors counts entries whose connector name arrived once the
	// map was full.
	otherConnectors int
}

func newMCPAuditLogSummary(interval time.Duration) *mcpAuditLogSummary {
	return &mcpAuditLogSummary{interval: interval, connectors: make(map[string]int)}
}

// mcpAuditLog summarizes this process's accepted MCP audit entries.
var mcpAuditLog = newMCPAuditLogSummary(mcpAuditLogInterval)

// reportMCPAuditWrite counts one entry the audit queue accepted and logs a
// summary line when one is due.
func reportMCPAuditWrite(entry MCPQueryAuditEntry) {
	if line := mcpAuditLog.record(entry, time.Now()); line != "" {
		log.Print(line)
	}
}

// record counts one accepted entry at now. It returns a summary line for the
// first entry after start, and after that for the first entry once the
// interval has passed since the previous line; otherwise it returns "". The
// counts restart after every line. Lines are driven by entries, so when
// traffic stops, the last counts are reported with the next entry.
func (s *mcpAuditLogSummary) record(entry MCPQueryAuditEntry, now time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.total++
	if entry.RequestBlocked {
		s.blocked++
	}
	if entry.ResponseRedacted {
		s.redacted++
	}
	if entry.ExfilExceeded {
		s.exfil++
	}
	name := entry.ConnectorName
	if len(name) > mcpAuditLogMaxNameLen {
		name = name[:mcpAuditLogMaxNameLen]
	}
	if _, known := s.connectors[name]; known || len(s.connectors) < mcpAuditLogMaxConnectors {
		s.connectors[name]++
	} else {
		s.otherConnectors++
	}

	var window string
	switch {
	case s.lastLine.IsZero():
		window = fmt.Sprintf("(first since start; later entries are summarized every %s)", s.interval)
	case now.Sub(s.lastLine) >= s.interval:
		window = "in the last " + now.Sub(s.lastLine).Round(time.Second).String()
	default:
		return ""
	}

	names := make([]string, 0, len(s.connectors))
	for n := range s.connectors {
		names = append(names, n)
	}
	sort.Strings(names)
	counts := make([]string, 0, len(names)+1)
	for _, n := range names {
		// Quoted, so a name can neither break the line nor pass for "other".
		counts = append(counts, fmt.Sprintf("%q=%d", n, s.connectors[n]))
	}
	if s.otherConnectors > 0 {
		counts = append(counts, fmt.Sprintf("other=%d", s.otherConnectors))
	}
	noun := "entries"
	if s.total == 1 {
		noun = "entry"
	}
	line := fmt.Sprintf("[MCP Audit] Logged %d %s %s: blocked=%d, redacted=%d, exfil=%d, connectors: %s",
		s.total, noun, window, s.blocked, s.redacted, s.exfil, strings.Join(counts, ", "))

	s.lastLine = now
	s.total, s.blocked, s.redacted, s.exfil, s.otherConnectors = 0, 0, 0, 0, 0
	clear(s.connectors)
	return line
}
