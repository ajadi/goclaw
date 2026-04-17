package pancake

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/metrics"
)

// counterValue reads the Prometheus counter for
// pancake_private_reply_total{page_id,result,reason}. Uses the default
// registry via testutil.CollectAndCount-style gather.
func counterValue(t *testing.T, page, result, reason string) float64 {
	t.Helper()
	mf, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, m := range mf {
		if m.GetName() != "pancake_private_reply_total" {
			continue
		}
		for _, metric := range m.GetMetric() {
			matched := 0
			for _, lp := range metric.GetLabel() {
				switch lp.GetName() {
				case "page_id":
					if lp.GetValue() == page {
						matched++
					}
				case "result":
					if lp.GetValue() == result {
						matched++
					}
				case "reason":
					if lp.GetValue() == reason {
						matched++
					}
				}
			}
			if matched == 3 {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// pageForTest keeps each test's counters isolated by using a unique page_id
// label per test. Avoids touching the global registry reset helpers.
func pageForTest(t *testing.T) string {
	t.Helper()
	return "metric-test-" + t.Name()
}

func seedCountersPage(t *testing.T) string {
	return pageForTest(t)
}

func TestMetrics_SentIncrement(t *testing.T) {
	cfg := pancakeInstanceConfig{}
	cfg.Features.PrivateReply = true
	cfg.PrivateReplyMessage = "hi"
	ch, _, _ := newChannelWithMultiCaptureAndStore(t, cfg)
	ch.pageID = seedCountersPage(t)

	before := counterValue(t, ch.pageID, string(metrics.ResultSent), "")
	ch.sendPrivateReply(context.Background(), "user-1", "conv-1", "", "")
	after := counterValue(t, ch.pageID, string(metrics.ResultSent), "")
	if after-before != 1 {
		t.Errorf("sent counter delta = %v; want 1", after-before)
	}
}

func TestMetrics_SkippedFeatureOff(t *testing.T) {
	cfg := pancakeInstanceConfig{} // feature off
	ch, _, _ := newChannelWithMultiCaptureAndStore(t, cfg)
	ch.pageID = seedCountersPage(t)

	before := counterValue(t, ch.pageID, string(metrics.ResultSkipped), string(metrics.ReasonFeatureOff))
	ch.sendPrivateReply(context.Background(), "user-1", "conv-1", "", "")
	after := counterValue(t, ch.pageID, string(metrics.ResultSkipped), string(metrics.ReasonFeatureOff))
	if after-before != 1 {
		t.Errorf("feature_off skip counter delta = %v; want 1", after-before)
	}
}

func TestMetrics_SkippedStoreUnwired(t *testing.T) {
	cfg := pancakeInstanceConfig{}
	cfg.Features.PrivateReply = true
	cfg.PageID = "page-123"
	ch, err := New(cfg, pancakeCreds{APIKey: "k", PageAccessToken: "t"}, bus.New(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch.pageID = seedCountersPage(t)

	before := counterValue(t, ch.pageID, string(metrics.ResultSkipped), string(metrics.ReasonStoreUnwired))
	ch.sendPrivateReply(context.Background(), "user-1", "conv-1", "", "")
	after := counterValue(t, ch.pageID, string(metrics.ResultSkipped), string(metrics.ReasonStoreUnwired))
	if after-before != 1 {
		t.Errorf("store_unwired skip counter delta = %v; want 1", after-before)
	}
}

func TestMetrics_SkippedScopeFilter(t *testing.T) {
	cfg := pancakeInstanceConfig{}
	cfg.Features.PrivateReply = true
	cfg.PrivateReplyOptions = &PrivateReplyOptions{DenyPostIDs: []string{"bad"}}
	ch, _, _ := newChannelWithMultiCaptureAndStore(t, cfg)
	ch.pageID = seedCountersPage(t)

	before := counterValue(t, ch.pageID, string(metrics.ResultSkipped), string(metrics.ReasonScopeFilter))
	ch.sendPrivateReply(context.Background(), "user-1", "conv-1", "bad", "")
	after := counterValue(t, ch.pageID, string(metrics.ResultSkipped), string(metrics.ReasonScopeFilter))
	if after-before != 1 {
		t.Errorf("scope_filter skip counter delta = %v; want 1", after-before)
	}
}

func TestMetrics_SkippedDedupHit(t *testing.T) {
	cfg := pancakeInstanceConfig{}
	cfg.Features.PrivateReply = true
	ch, _, fake := newChannelWithMultiCaptureAndStore(t, cfg)
	ch.pageID = seedCountersPage(t)
	if err := fake.MarkSent(context.Background(), ch.pageID, "user-1"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	before := counterValue(t, ch.pageID, string(metrics.ResultSkipped), string(metrics.ReasonDedupHit))
	ch.sendPrivateReply(context.Background(), "user-1", "conv-1", "", "")
	after := counterValue(t, ch.pageID, string(metrics.ResultSkipped), string(metrics.ReasonDedupHit))
	if after-before != 1 {
		t.Errorf("dedup_hit skip counter delta = %v; want 1", after-before)
	}
}

func TestMetrics_SkippedDedupError(t *testing.T) {
	cfg := pancakeInstanceConfig{}
	cfg.Features.PrivateReply = true
	ch, _, fake := newChannelWithMultiCaptureAndStore(t, cfg)
	ch.pageID = seedCountersPage(t)
	fake.tryClaimErr = errors.New("boom")

	before := counterValue(t, ch.pageID, string(metrics.ResultSkipped), string(metrics.ReasonDedupError))
	ch.sendPrivateReply(context.Background(), "user-1", "conv-1", "", "")
	after := counterValue(t, ch.pageID, string(metrics.ResultSkipped), string(metrics.ReasonDedupError))
	if after-before != 1 {
		t.Errorf("dedup_error skip counter delta = %v; want 1", after-before)
	}
}

func TestMetrics_FailedAPIError(t *testing.T) {
	errorTransport := &captureTransport{
		resp: &http.Response{
			StatusCode: 500,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("error")),
		},
	}
	cfg := pancakeInstanceConfig{}
	cfg.Features.PrivateReply = true
	cfg.PageID = "page-123"
	ch, err := New(cfg, pancakeCreds{APIKey: "k", PageAccessToken: "t"}, bus.New(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch.apiClient.httpClient = &http.Client{Transport: errorTransport}
	fake := newFakePrivateReplyStore()
	fake.SkipTenantCheck = true
	ch.privateReplyStore = fake
	ch.pageID = seedCountersPage(t)

	before := counterValue(t, ch.pageID, string(metrics.ResultFailed), string(metrics.ReasonAPIError))
	ch.sendPrivateReply(context.Background(), "user-1", "conv-1", "", "")
	after := counterValue(t, ch.pageID, string(metrics.ResultFailed), string(metrics.ReasonAPIError))
	if after-before != 1 {
		t.Errorf("api_error failed counter delta = %v; want 1", after-before)
	}
}
