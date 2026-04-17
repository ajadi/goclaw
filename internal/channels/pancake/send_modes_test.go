package pancake

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// collectActions reads transport bodies and extracts the "action" field from
// each to make per-call dispatch assertions readable.
func collectActions(t *testing.T, transport *multiCaptureTransport) []string {
	t.Helper()
	transport.mu.Lock()
	defer transport.mu.Unlock()
	out := make([]string, 0, len(transport.bodies))
	for _, b := range transport.bodies {
		var p map[string]any
		_ = json.Unmarshal(b, &p)
		if a, ok := p["action"].(string); ok {
			out = append(out, a)
		}
	}
	return out
}

func TestSendCommentReply_AfterReplyMode(t *testing.T) {
	cfg := pancakeInstanceConfig{}
	cfg.Features.PrivateReply = true
	cfg.PrivateReplyMessage = "Hi"
	ch, transport, _ := newChannelWithMultiCaptureAndStore(t, cfg)

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "conv-1",
		Content: "public",
		Metadata: map[string]string{
			"pancake_mode":        "comment",
			"reply_to_comment_id": "msg-1",
			"sender_id":           "user-1",
			"private_reply_mode":  "after_reply",
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	actions := collectActions(t, transport)
	if len(actions) != 2 || actions[0] != "reply_comment" || actions[1] != "private_reply" {
		t.Fatalf("actions = %v; want [reply_comment, private_reply]", actions)
	}
}

func TestSendCommentReply_StandaloneMode_SkipsPublicReply(t *testing.T) {
	cfg := pancakeInstanceConfig{}
	cfg.Features.PrivateReply = true
	ch, transport, _ := newChannelWithMultiCaptureAndStore(t, cfg)

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "conv-1",
		Content: "pipeline-reply-ignored",
		Metadata: map[string]string{
			"pancake_mode":        "comment",
			"sender_id":           "user-1",
			"private_reply_mode":  "standalone",
			"private_reply_only":  "true",
			// reply_to_comment_id intentionally missing — standalone must not fail on it.
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	actions := collectActions(t, transport)
	if len(actions) != 1 || actions[0] != "private_reply" {
		t.Fatalf("actions = %v; want [private_reply]", actions)
	}
}

func TestSendCommentReply_AfterReplyStillRequiresCommentID(t *testing.T) {
	// Regression: hoisting the guard into the non-standalone branch must not
	// accidentally remove it — after_reply without reply_to_comment_id must fail.
	cfg := pancakeInstanceConfig{}
	cfg.Features.PrivateReply = true
	ch, _, _ := newChannelWithMultiCaptureAndStore(t, cfg)

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "conv-1",
		Content: "public",
		Metadata: map[string]string{
			"pancake_mode":       "comment",
			"sender_id":          "user-1",
			"private_reply_mode": "after_reply",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "reply_to_comment_id") {
		t.Fatalf("want reply_to_comment_id error; got %v", err)
	}
}

func TestSendPrivateReply_ScopeFilterDeny(t *testing.T) {
	cfg := pancakeInstanceConfig{}
	cfg.Features.PrivateReply = true
	cfg.PrivateReplyOptions = &PrivateReplyOptions{DenyPostIDs: []string{"banned-post"}}
	ch, transport, fake := newChannelWithMultiCaptureAndStore(t, cfg)

	ch.sendPrivateReply(context.Background(), "user-1", "conv-1", "banned-post", "Tuan")

	if len(transport.bodies) != 0 {
		t.Fatalf("expected no API call; got %d", len(transport.bodies))
	}
	_, mark, _ := fake.stats()
	if mark != 0 {
		t.Errorf("MarkSent calls = %d; want 0 (filtered pre-send)", mark)
	}
}

func TestSendPrivateReply_ScopeFilterAllowMiss(t *testing.T) {
	cfg := pancakeInstanceConfig{}
	cfg.Features.PrivateReply = true
	cfg.PrivateReplyOptions = &PrivateReplyOptions{AllowPostIDs: []string{"p-allow"}}
	ch, transport, _ := newChannelWithMultiCaptureAndStore(t, cfg)

	ch.sendPrivateReply(context.Background(), "user-1", "conv-1", "other-post", "Tuan")

	if len(transport.bodies) != 0 {
		t.Fatalf("expected no API call; got %d", len(transport.bodies))
	}
}

func TestSendPrivateReply_DedupHit_SkipsSend(t *testing.T) {
	cfg := pancakeInstanceConfig{}
	cfg.Features.PrivateReply = true
	ch, transport, fake := newChannelWithMultiCaptureAndStore(t, cfg)

	// Pre-populate: treat as already sent.
	ctx := context.Background()
	if err := fake.MarkSent(ctx, ch.pageID, "user-1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fake.markSentCalls = 0 // reset counter after seed

	ch.sendPrivateReply(ctx, "user-1", "conv-1", "post-1", "Tuan")

	if len(transport.bodies) != 0 {
		t.Fatalf("expected no API call when dedup says sent; got %d", len(transport.bodies))
	}
	if fake.markSentCalls != 0 {
		t.Errorf("MarkSent called %d times after dedup hit; want 0", fake.markSentCalls)
	}
}

func TestSendPrivateReply_DedupMiss_SendsAndMarks(t *testing.T) {
	cfg := pancakeInstanceConfig{}
	cfg.Features.PrivateReply = true
	cfg.PrivateReplyMessage = "Hello"
	ch, transport, fake := newChannelWithMultiCaptureAndStore(t, cfg)

	ch.sendPrivateReply(context.Background(), "user-1", "conv-1", "", "")

	if len(transport.bodies) != 1 {
		t.Fatalf("expected 1 API call; got %d", len(transport.bodies))
	}
	// TryClaim replaces WasSent+MarkSent in one round trip — the slot is
	// marked via the claim itself, not a separate MarkSent call.
	if fake.tryClaimCalls != 1 {
		t.Errorf("TryClaim calls = %d; want 1", fake.tryClaimCalls)
	}
}

func TestSendPrivateReply_DBError_FailsClosed(t *testing.T) {
	cfg := pancakeInstanceConfig{}
	cfg.Features.PrivateReply = true
	ch, transport, fake := newChannelWithMultiCaptureAndStore(t, cfg)
	fake.tryClaimErr = errors.New("boom")

	ch.sendPrivateReply(context.Background(), "user-1", "conv-1", "", "")

	if len(transport.bodies) != 0 {
		t.Fatalf("expected no API call on DB error (fail-closed); got %d", len(transport.bodies))
	}
}

func TestSendPrivateReply_TemplateRendering(t *testing.T) {
	cfg := pancakeInstanceConfig{}
	cfg.Features.PrivateReply = true
	cfg.PrivateReplyMessage = "Hi {{commenter_name}}"
	ch, transport, _ := newChannelWithMultiCaptureAndStore(t, cfg)

	ch.sendPrivateReply(context.Background(), "user-1", "conv-1", "", "Tuấn")

	if len(transport.bodies) != 1 {
		t.Fatalf("expected 1 API call; got %d", len(transport.bodies))
	}
	var body map[string]any
	_ = json.Unmarshal(transport.bodies[0], &body)
	if body["message"] != "Hi Tuấn" {
		t.Errorf("message = %v; want 'Hi Tuấn'", body["message"])
	}
}

func TestSendPrivateReply_FeatureDisabled(t *testing.T) {
	cfg := pancakeInstanceConfig{}
	cfg.Features.PrivateReply = false
	ch, transport, _ := newChannelWithMultiCaptureAndStore(t, cfg)

	ch.sendPrivateReply(context.Background(), "user-1", "conv-1", "", "")

	if len(transport.bodies) != 0 {
		t.Fatalf("expected no API call with feature off; got %d", len(transport.bodies))
	}
}

func TestSendPrivateReply_StoreNotWired_Silent(t *testing.T) {
	// Backward-compat: Factory (no stores) leaves store nil. Feature must
	// silently no-op rather than panic.
	cfg := pancakeInstanceConfig{}
	cfg.Features.PrivateReply = true
	cfg.PageID = "page-123"
	creds := pancakeCreds{APIKey: "k", PageAccessToken: "t"}
	ch, err := New(cfg, creds, bus.New(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	transport := &multiCaptureTransport{}
	ch.apiClient.httpClient = &http.Client{Transport: transport}

	ch.sendPrivateReply(context.Background(), "user-1", "conv-1", "", "")

	if len(transport.bodies) != 0 {
		t.Fatalf("expected no API call without wired store; got %d", len(transport.bodies))
	}
}

// --- handleCommentEvent standalone bypass tests ---

func TestHandleCommentEvent_StandaloneBypassesKeywordFilter(t *testing.T) {
	cfg := pancakeInstanceConfig{}
	cfg.Features.PrivateReply = true
	cfg.PrivateReplyMode = "standalone"
	cfg.CommentReplyOptions.Filter = "keyword"
	cfg.CommentReplyOptions.Keywords = []string{"giá"}
	ch, _, _ := newChannelWithMultiCaptureAndStore(t, cfg)

	// Comment that does NOT match the keyword — standalone must still fire.
	before := time.Now()
	ch.handleCommentEvent(MessagingData{
		PageID:         "page-123",
		ConversationID: "conv-1",
		Type:           "COMMENT",
		PostID:         "p1",
		Message: MessagingMessage{
			ID:         "msg-1",
			Content:    "chào shop",
			SenderID:   "user-1",
			SenderName: "Tuan",
		},
	})

	// Poll the outbound bus briefly — fast-path publishes directly.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	msg, ok := ch.Bus().SubscribeOutbound(ctx)
	if !ok {
		t.Fatalf("expected outbound publish via fast-path within 200ms (elapsed %v)", time.Since(before))
	}
	if msg.Metadata["private_reply_only"] != "true" {
		t.Errorf("private_reply_only = %q; want true", msg.Metadata["private_reply_only"])
	}
	if msg.Metadata["sender_id"] != "user-1" {
		t.Errorf("sender_id = %q; want user-1", msg.Metadata["sender_id"])
	}
}

func TestHandleCommentEvent_AfterReplyRespectsKeywordFilter(t *testing.T) {
	cfg := pancakeInstanceConfig{}
	cfg.Features.CommentReply = true
	cfg.Features.PrivateReply = true
	cfg.PrivateReplyMode = "after_reply"
	cfg.CommentReplyOptions.Filter = "keyword"
	cfg.CommentReplyOptions.Keywords = []string{"giá"}
	ch, _, _ := newChannelWithMultiCaptureAndStore(t, cfg)

	ch.handleCommentEvent(MessagingData{
		PageID:         "page-123",
		ConversationID: "conv-1",
		Type:           "COMMENT",
		PostID:         "p1",
		Message: MessagingMessage{
			ID:         "msg-1",
			Content:    "chào shop",
			SenderID:   "user-1",
			SenderName: "Tuan",
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, ok := ch.Bus().SubscribeOutbound(ctx); ok {
		t.Error("after_reply mode must NOT bypass keyword filter")
	}
	// Also ensure no inbound was emitted (pipeline skipped due to filter).
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	if _, ok := ch.Bus().ConsumeInbound(ctx2); ok {
		t.Error("after_reply mode must NOT publish inbound when filter rejects")
	}
}

func TestHandleCommentEvent_StandaloneRequiresPrivateReplyFeature(t *testing.T) {
	cfg := pancakeInstanceConfig{}
	cfg.PrivateReplyMode = "standalone"
	// PrivateReply feature off — mode alone is not enough.
	ch, _, _ := newChannelWithMultiCaptureAndStore(t, cfg)

	ch.handleCommentEvent(MessagingData{
		PageID:         "page-123",
		ConversationID: "conv-1",
		Type:           "COMMENT",
		PostID:         "p1",
		Message: MessagingMessage{
			ID:         "msg-1",
			Content:    "hello",
			SenderID:   "user-1",
			SenderName: "Tuan",
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, ok := ch.Bus().SubscribeOutbound(ctx); ok {
		t.Error("standalone without PrivateReply feature must not publish outbound")
	}
}

func TestHandleCommentEvent_StandaloneDoesntRequireCommentReply(t *testing.T) {
	cfg := pancakeInstanceConfig{}
	cfg.Features.CommentReply = false
	cfg.Features.PrivateReply = true
	cfg.PrivateReplyMode = "standalone"
	ch, _, _ := newChannelWithMultiCaptureAndStore(t, cfg)

	ch.handleCommentEvent(MessagingData{
		PageID:         "page-123",
		ConversationID: "conv-1",
		Type:           "COMMENT",
		PostID:         "p1",
		Message: MessagingMessage{
			ID:         "msg-1",
			Content:    "hello",
			SenderID:   "user-1",
			SenderName: "Tuan",
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	msg, ok := ch.Bus().SubscribeOutbound(ctx)
	if !ok {
		t.Fatal("standalone with comment_reply off must still publish outbound")
	}
	if msg.Metadata["private_reply_only"] != "true" {
		t.Errorf("private_reply_only = %q; want true", msg.Metadata["private_reply_only"])
	}
}

// TestSend_InjectsTenantIntoContext is a regression guard for the critical
// production bug flagged in code review: sendPrivateReply → store.WasSent /
// TryClaim pulls tenant from ctx, but the Pancake outbound dispatch path
// never set one. Without this fix, the real PG/SQLite stores fail-closed with
// ErrMissingTenantID and the DM is silently dropped in production. Unit tests
// masked this by using fake.SkipTenantCheck=true.
func TestSend_InjectsTenantIntoContext(t *testing.T) {
	cfg := pancakeInstanceConfig{}
	cfg.Features.PrivateReply = true
	cfg.PrivateReplyMessage = "hi"
	ch, transport, fake := newChannelWithMultiCaptureAndStore(t, cfg)

	// Force the real tenant check: fake must reject ctx without tenant.
	fake.SkipTenantCheck = false
	expected := uuid.New()
	ch.SetTenantID(expected)

	// ctx WITHOUT WithTenantID — simulates dispatcher → channel.Send path.
	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "conv-1",
		Content: "public",
		Metadata: map[string]string{
			"pancake_mode":        "comment",
			"reply_to_comment_id": "msg-1",
			"sender_id":           "user-1",
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	// If Send injected tenant correctly, the store was reached with it.
	if fake.tryClaimCalls == 0 {
		t.Fatalf("TryClaim not called — Send did not route through private-reply store")
	}

	// Look up by (expected tenant, page, sender) directly — proves tenant
	// flowed through to the store.
	ctx := store.WithTenantID(context.Background(), expected)
	was, err := fake.WasSent(ctx, ch.pageID, "user-1", time.Hour)
	if err != nil {
		t.Fatalf("WasSent with real tenant check: %v", err)
	}
	if !was {
		t.Fatalf("expected row for (%s, %s, user-1); Send did not inject tenant", expected, ch.pageID)
	}

	// Also verify public + DM both went through.
	actions := collectActions(t, transport)
	if len(actions) != 2 || actions[0] != "reply_comment" || actions[1] != "private_reply" {
		t.Errorf("actions = %v; want [reply_comment, private_reply]", actions)
	}
}
