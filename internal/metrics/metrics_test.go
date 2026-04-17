package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestRecordPancakePrivateReply verifies counter labels and increment
// semantics. Tests reuse the global registry so label values are unique per
// test to avoid cross-test interference.
func TestRecordPancakePrivateReply(t *testing.T) {
	const page = "test-page-record"

	RecordPancakePrivateReply(page, ResultSent, ReasonNone)
	RecordPancakePrivateReply(page, ResultSent, ReasonNone)
	RecordPancakePrivateReply(page, ResultSkipped, ReasonDedupHit)
	RecordPancakePrivateReply(page, ResultFailed, ReasonAPIError)

	if got := testutil.ToFloat64(pancakePrivateReplyTotal.WithLabelValues(page, "sent", "")); got != 2 {
		t.Errorf("sent counter = %v; want 2", got)
	}
	if got := testutil.ToFloat64(pancakePrivateReplyTotal.WithLabelValues(page, "skipped", "dedup_hit")); got != 1 {
		t.Errorf("skipped dedup_hit counter = %v; want 1", got)
	}
	if got := testutil.ToFloat64(pancakePrivateReplyTotal.WithLabelValues(page, "failed", "api_error")); got != 1 {
		t.Errorf("failed api_error counter = %v; want 1", got)
	}
}

func TestRecordPancakePrivateReply_DistinctPagesDoNotCollide(t *testing.T) {
	RecordPancakePrivateReply("page-a-distinct", ResultSent, ReasonNone)
	RecordPancakePrivateReply("page-b-distinct", ResultSent, ReasonNone)
	RecordPancakePrivateReply("page-a-distinct", ResultSent, ReasonNone)

	if got := testutil.ToFloat64(pancakePrivateReplyTotal.WithLabelValues("page-a-distinct", "sent", "")); got != 2 {
		t.Errorf("page-a sent = %v; want 2", got)
	}
	if got := testutil.ToFloat64(pancakePrivateReplyTotal.WithLabelValues("page-b-distinct", "sent", "")); got != 1 {
		t.Errorf("page-b sent = %v; want 1", got)
	}
}

// TestPromhttpHandlerExposesCounter ensures our counter actually appears on a
// /metrics scrape — catches registry-mismatch bugs where counters register to
// a different registry than promhttp serves.
func TestPromhttpHandlerExposesCounter(t *testing.T) {
	RecordPancakePrivateReply("scrape-test-page", ResultSent, ReasonNone)

	srv := httptest.NewServer(promhttp.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	if !strings.Contains(text, "# HELP pancake_private_reply_total") {
		t.Errorf("scrape missing HELP line for pancake_private_reply_total")
	}
	if !strings.Contains(text, `pancake_private_reply_total{page_id="scrape-test-page",reason="",result="sent"} 1`) {
		t.Errorf("scrape missing expected sample line; body excerpt:\n%s", headLines(text, 40))
	}
}

func headLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
