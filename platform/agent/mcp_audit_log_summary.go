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

// mcpAuditLogInterval is how often successful MCP audit writes are logged.
//
// Every governed check (check-input, check-output, connector query/execute)
// writes one mcp_query_audits row, and logging each write repeated that row
// in the log stream. On a fleet whose audit plane redacts every stored
// telemetry payload through check-output, that was one to five lines a
// second, and it flooded Datadog. One summary line per interval keeps the
// signal (audit writes are flowing, and how many were blocked, redacted or
// over an exfiltration limit) at a fixed, small volume. Failed writes are
// still logged on every call.
const mcpAuditLogInterval = 5 * time.Minute

// mcpAuditLogMaxConnectors bounds the per-connector counts in one summary.
// Connector names come from the request (connector_type), so names past the
// bound are counted under "other" instead of growing the map and the log line
// with every new name a caller sends.
const mcpAuditLogMaxConnectors = 20

// mcpAuditLogSummary counts successful MCP audit writes between log lines.
type mcpAuditLogSummary struct {
	mu       sync.Mutex
	interval time.Duration
	// lastLine is zero until the first line, so the first write after start
	// is logged at once.
	lastLine   time.Time
	total      int
	blocked    int
	redacted   int
	exfil      int
	connectors map[string]int
}

func newMCPAuditLogSummary(interval time.Duration) *mcpAuditLogSummary {
	return &mcpAuditLogSummary{interval: interval, connectors: make(map[string]int)}
}

// mcpAuditLog summarizes this process's successful MCP audit writes.
var mcpAuditLog = newMCPAuditLogSummary(mcpAuditLogInterval)

// reportMCPAuditWrite counts one successful audit write and logs a summary
// line when one is due.
func reportMCPAuditWrite(entry MCPQueryAuditEntry) {
	if line := mcpAuditLog.record(entry, time.Now()); line != "" {
		log.Print(line)
	}
}

// record counts one successful write made at now. It returns a summary line
// for the first write after start, and after that for the first write once
// the interval has passed since the previous line; otherwise it returns "".
// The counts restart after every line. Lines are driven by writes, so when
// traffic stops, the last counts are reported with the next write; the rows
// themselves are already in mcp_query_audits.
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
	if name == "" {
		name = "(none)"
	}
	if _, known := s.connectors[name]; !known && len(s.connectors) >= mcpAuditLogMaxConnectors {
		name = "other"
	}
	s.connectors[name]++

	var window string
	switch {
	case s.lastLine.IsZero():
		window = fmt.Sprintf("(first since start; later writes are summarized every %s)", s.interval)
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
	counts := make([]string, len(names))
	for i, n := range names {
		counts[i] = fmt.Sprintf("%s=%d", n, s.connectors[n])
	}
	noun := "entries"
	if s.total == 1 {
		noun = "entry"
	}
	line := fmt.Sprintf("[MCP Audit] Logged %d %s %s: blocked=%d, redacted=%d, exfil=%d, connectors: %s",
		s.total, noun, window, s.blocked, s.redacted, s.exfil, strings.Join(counts, ", "))

	s.lastLine = now
	s.total, s.blocked, s.redacted, s.exfil = 0, 0, 0, 0
	clear(s.connectors)
	return line
}
