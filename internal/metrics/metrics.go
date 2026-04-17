// Package metrics exposes lightweight Prometheus counters for feature-level
// observability. The package registers collectors against the default
// Prometheus registry; cmd/gateway mounts promhttp.Handler() at /metrics.
//
// Intentionally narrow: only counters the operations team has asked for. Add
// a collector here when a concrete monitoring need lands — not preemptively.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// PancakePrivateReplyResult is the label value for private_reply dispositions.
type PancakePrivateReplyResult string

const (
	ResultSent    PancakePrivateReplyResult = "sent"    // DM dispatched + MarkSent
	ResultSkipped PancakePrivateReplyResult = "skipped" // filtered/deduped — no API call
	ResultFailed  PancakePrivateReplyResult = "failed"  // API call returned error
)

// PancakePrivateReplyReason distinguishes why a send was skipped (sent/failed
// pass "" for the reason label).
type PancakePrivateReplyReason string

const (
	ReasonNone          PancakePrivateReplyReason = ""
	ReasonFeatureOff    PancakePrivateReplyReason = "feature_off"
	ReasonStoreUnwired  PancakePrivateReplyReason = "store_unwired"
	ReasonScopeFilter   PancakePrivateReplyReason = "scope_filter"
	ReasonDedupHit      PancakePrivateReplyReason = "dedup_hit"
	ReasonDedupError    PancakePrivateReplyReason = "dedup_error"
	ReasonAPIError      PancakePrivateReplyReason = "api_error"
	ReasonMarkError     PancakePrivateReplyReason = "mark_error"
)

var pancakePrivateReplyTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "pancake_private_reply_total",
		Help: "Pancake private-reply DM dispositions. Labels: page_id, result (sent|skipped|failed), reason.",
	},
	[]string{"page_id", "result", "reason"},
)

// RecordPancakePrivateReply increments the counter for the given page and
// disposition. Safe to call from hot paths — counter ops are lock-free.
func RecordPancakePrivateReply(pageID string, result PancakePrivateReplyResult, reason PancakePrivateReplyReason) {
	pancakePrivateReplyTotal.WithLabelValues(pageID, string(result), string(reason)).Inc()
}
