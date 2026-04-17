//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels/pancake"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/store/pg"
)

// --- Capture transport shared by all e2e tests ---

type pancakeTransport struct {
	mu        sync.Mutex
	bodies    [][]byte
	responses map[string]int // action → HTTP status; default 200
}

func newPancakeTransport() *pancakeTransport {
	return &pancakeTransport{responses: map[string]int{}}
}

func (t *pancakeTransport) forceStatus(action string, status int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.responses[action] = status
}

func (t *pancakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	var parsed map[string]any
	if len(body) > 0 {
		_ = json.Unmarshal(body, &parsed)
	}
	action, _ := parsed["action"].(string)

	t.mu.Lock()
	// Only capture outgoing request writes (have action), not GETs like /posts.
	if action != "" {
		t.bodies = append(t.bodies, body)
	}
	status, ok := t.responses[action]
	t.mu.Unlock()
	if !ok {
		status = http.StatusOK
	}

	// GETs like /pages/{id}/posts expect {"data": [...]} shape; POSTs accept {"success":true}.
	respBody := `{"success":true}`
	if strings.Contains(req.URL.Path, "/posts") && req.Method == http.MethodGet {
		respBody = `{"data":[]}`
	}
	if status >= 400 {
		respBody = `{"error":{"code":400,"message":"simulated"}}`
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(respBody)),
		Request:    req,
	}, nil
}

func (t *pancakeTransport) actions() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.bodies))
	for _, b := range t.bodies {
		var p map[string]any
		_ = json.Unmarshal(b, &p)
		if a, ok := p["action"].(string); ok {
			out = append(out, a)
		}
	}
	return out
}

func (t *pancakeTransport) countAction(name string) int {
	n := 0
	for _, a := range t.actions() {
		if a == name {
			n++
		}
	}
	return n
}

// --- Helpers ---

type pancakeCreds struct {
	APIKey          string `json:"api_key"`
	PageAccessToken string `json:"page_access_token"`
}

type pancakeFeatures struct {
	InboxReply   bool `json:"inbox_reply"`
	CommentReply bool `json:"comment_reply"`
	PrivateReply bool `json:"private_reply"`
}

type pancakeCfg struct {
	PageID              string                       `json:"page_id"`
	Platform            string                       `json:"platform"`
	Features            pancakeFeatures              `json:"features"`
	PrivateReplyMessage string                       `json:"private_reply_message,omitempty"`
	PrivateReplyMode    string                       `json:"private_reply_mode,omitempty"`
	PrivateReplyTTLDays int                          `json:"private_reply_ttl_days,omitempty"`
	PrivateReplyOptions *pancakePrivateReplyOpts     `json:"private_reply_options,omitempty"`
	CommentReplyOptions *pancakeCommentReplyOpts     `json:"comment_reply_options,omitempty"`
}

type pancakePrivateReplyOpts struct {
	AllowPostIDs []string `json:"allow_post_ids,omitempty"`
	DenyPostIDs  []string `json:"deny_post_ids,omitempty"`
}

type pancakeCommentReplyOpts struct {
	Filter   string   `json:"filter,omitempty"`
	Keywords []string `json:"keywords,omitempty"`
}

func newPancakeE2E(t *testing.T, tenantID uuid.UUID, transport *pancakeTransport, cfg pancakeCfg) (channel pancakeChannelHandle, cleanup func()) {
	t.Helper()

	db := testDB(t)

	prStore := pg.NewPancakePrivateReplyStore(db)

	creds, err := json.Marshal(pancakeCreds{APIKey: "k", PageAccessToken: "t"})
	if err != nil {
		t.Fatalf("marshal creds: %v", err)
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal cfg: %v", err)
	}

	factory := pancake.FactoryWithStores(prStore)
	msgBus := bus.New()
	ch, err := factory("pancake-e2e", creds, cfgJSON, msgBus, nil)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}

	// Reach inside to swap the HTTP client. Access via exported hook.
	if setter, ok := ch.(interface {
		SetHTTPClientForTest(*http.Client)
	}); ok {
		setter.SetHTTPClientForTest(&http.Client{Transport: transport})
	} else {
		t.Fatalf("pancake Channel missing SetHTTPClientForTest hook — add it or adjust test")
	}

	cleanup = func() {
		db.Exec(`DELETE FROM pancake_private_reply_sent WHERE tenant_id = $1`, tenantID)
	}
	t.Cleanup(cleanup)
	return pancakeChannelHandle{raw: ch, tenantID: tenantID, msgBus: msgBus}, cleanup
}

type pancakeChannelHandle struct {
	raw      any // channels.Channel
	tenantID uuid.UUID
	msgBus   *bus.MessageBus
}

func (h pancakeChannelHandle) Send(ctx context.Context, msg bus.OutboundMessage) error {
	sender, ok := h.raw.(interface {
		Send(context.Context, bus.OutboundMessage) error
	})
	if !ok {
		return fmt.Errorf("channel does not implement Send")
	}
	return sender.Send(store.WithTenantID(ctx, h.tenantID), msg)
}

// --- Tests ---

func TestPancakePrivateReply_E2E_HappyPathPersists(t *testing.T) {
	tenantID := seedTenantOnly(t)
	transport := newPancakeTransport()

	ch, _ := newPancakeE2E(t, tenantID, transport, pancakeCfg{
		PageID:              "page-e2e",
		Platform:            "facebook",
		Features:            pancakeFeatures{CommentReply: true, PrivateReply: true},
		PrivateReplyMessage: "Hi {{commenter_name}}",
	})

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "conv-1",
		Content: "public reply",
		Metadata: map[string]string{
			"pancake_mode":        "comment",
			"reply_to_comment_id": "msg-1",
			"sender_id":           "user-1",
			"post_id":             "post-1",
			"display_name":        "Tuan",
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if transport.countAction("reply_comment") != 1 {
		t.Errorf("reply_comment = %d; want 1", transport.countAction("reply_comment"))
	}
	if transport.countAction("private_reply") != 1 {
		t.Errorf("private_reply = %d; want 1", transport.countAction("private_reply"))
	}

	// DB row must exist.
	db := testDB(t)
	var rows int
	_ = db.QueryRow(
		`SELECT COUNT(*) FROM pancake_private_reply_sent
		 WHERE tenant_id = $1 AND page_id = 'page-e2e' AND sender_id = 'user-1'`,
		tenantID,
	).Scan(&rows)
	if rows != 1 {
		t.Errorf("DB row count = %d; want 1", rows)
	}
}

func TestPancakePrivateReply_E2E_DedupSurvivesRestart(t *testing.T) {
	tenantID := seedTenantOnly(t)
	transport := newPancakeTransport()

	cfg := pancakeCfg{
		PageID:   "page-e2e",
		Platform: "facebook",
		Features: pancakeFeatures{CommentReply: true, PrivateReply: true},
	}

	// First channel instance — sends initial DM.
	ch1, _ := newPancakeE2E(t, tenantID, transport, cfg)
	err := ch1.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "conv-1",
		Content: "hi",
		Metadata: map[string]string{
			"pancake_mode":        "comment",
			"reply_to_comment_id": "msg-1",
			"sender_id":           "user-1",
		},
	})
	if err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	if transport.countAction("private_reply") != 1 {
		t.Fatalf("initial private_reply count = %d; want 1", transport.countAction("private_reply"))
	}

	// Fresh transport & channel instance simulate restart — same PG store.
	transport2 := newPancakeTransport()
	ch2, _ := newPancakeE2E(t, tenantID, transport2, cfg)
	err = ch2.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "conv-2",
		Content: "hi again",
		Metadata: map[string]string{
			"pancake_mode":        "comment",
			"reply_to_comment_id": "msg-2",
			"sender_id":           "user-1",
		},
	})
	if err != nil {
		t.Fatalf("Send 2: %v", err)
	}

	if transport2.countAction("reply_comment") != 1 {
		t.Errorf("post-restart reply_comment = %d; want 1", transport2.countAction("reply_comment"))
	}
	if transport2.countAction("private_reply") != 0 {
		t.Errorf("post-restart private_reply = %d; want 0 (dedup must hold)", transport2.countAction("private_reply"))
	}
}

func TestPancakePrivateReply_E2E_TTLExpiryAllowsResend(t *testing.T) {
	tenantID := seedTenantOnly(t)
	transport := newPancakeTransport()

	ch, _ := newPancakeE2E(t, tenantID, transport, pancakeCfg{
		PageID:              "page-e2e",
		Platform:            "facebook",
		Features:            pancakeFeatures{CommentReply: true, PrivateReply: true},
		PrivateReplyTTLDays: 7,
	})

	// First send marks the row.
	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "conv-1",
		Content: "hi",
		Metadata: map[string]string{
			"pancake_mode":        "comment",
			"reply_to_comment_id": "msg-1",
			"sender_id":           "cuong",
		},
	})
	if err != nil {
		t.Fatalf("Send 1: %v", err)
	}

	// Fast-forward sent_at.
	db := testDB(t)
	_, err = db.Exec(
		`UPDATE pancake_private_reply_sent SET sent_at = NOW() - INTERVAL '8 days'
		 WHERE tenant_id = $1 AND sender_id = 'cuong'`,
		tenantID,
	)
	if err != nil {
		t.Fatalf("backdate: %v", err)
	}

	err = ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "conv-1",
		Content: "hi again",
		Metadata: map[string]string{
			"pancake_mode":        "comment",
			"reply_to_comment_id": "msg-2",
			"sender_id":           "cuong",
		},
	})
	if err != nil {
		t.Fatalf("Send 2: %v", err)
	}

	if transport.countAction("private_reply") != 2 {
		t.Errorf("private_reply after TTL expiry = %d; want 2", transport.countAction("private_reply"))
	}
}

func TestPancakePrivateReply_E2E_ScopeFilterDenyPostID(t *testing.T) {
	tenantID := seedTenantOnly(t)
	transport := newPancakeTransport()

	ch, _ := newPancakeE2E(t, tenantID, transport, pancakeCfg{
		PageID:   "page-e2e",
		Platform: "facebook",
		Features: pancakeFeatures{CommentReply: true, PrivateReply: true},
		PrivateReplyOptions: &pancakePrivateReplyOpts{
			DenyPostIDs: []string{"post-spam"},
		},
	})

	// Denied post — no DM.
	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "conv-1",
		Content: "hi",
		Metadata: map[string]string{
			"pancake_mode":        "comment",
			"reply_to_comment_id": "msg-1",
			"sender_id":           "user-a",
			"post_id":             "post-spam",
		},
	})
	if err != nil {
		t.Fatalf("Send denied: %v", err)
	}

	// Allowed post — DM fires.
	err = ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "conv-2",
		Content: "hi",
		Metadata: map[string]string{
			"pancake_mode":        "comment",
			"reply_to_comment_id": "msg-2",
			"sender_id":           "user-b",
			"post_id":             "post-ok",
		},
	})
	if err != nil {
		t.Fatalf("Send allowed: %v", err)
	}

	if transport.countAction("reply_comment") != 2 {
		t.Errorf("reply_comment = %d; want 2", transport.countAction("reply_comment"))
	}
	if transport.countAction("private_reply") != 1 {
		t.Errorf("private_reply = %d; want 1 (spam post filtered)", transport.countAction("private_reply"))
	}
}

func TestPancakePrivateReply_E2E_FBPolicyError_NoMark(t *testing.T) {
	tenantID := seedTenantOnly(t)
	transport := newPancakeTransport()
	transport.forceStatus("private_reply", http.StatusBadRequest)

	ch, _ := newPancakeE2E(t, tenantID, transport, pancakeCfg{
		PageID:   "page-e2e",
		Platform: "facebook",
		Features: pancakeFeatures{CommentReply: true, PrivateReply: true},
	})

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "conv-1",
		Content: "hi",
		Metadata: map[string]string{
			"pancake_mode":        "comment",
			"reply_to_comment_id": "msg-1",
			"sender_id":           "user-fail",
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	// DB row must NOT exist — retry must be allowed on next comment.
	db := testDB(t)
	var rows int
	_ = db.QueryRow(
		`SELECT COUNT(*) FROM pancake_private_reply_sent
		 WHERE tenant_id = $1 AND sender_id = 'user-fail'`,
		tenantID,
	).Scan(&rows)
	if rows != 0 {
		t.Errorf("DB row count = %d; want 0 (must not mark sent on API error)", rows)
	}

	// Flip API to success — retry succeeds.
	transport.forceStatus("private_reply", http.StatusOK)
	err = ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "conv-1",
		Content: "hi again",
		Metadata: map[string]string{
			"pancake_mode":        "comment",
			"reply_to_comment_id": "msg-2",
			"sender_id":           "user-fail",
		},
	})
	if err != nil {
		t.Fatalf("Send retry: %v", err)
	}

	_ = db.QueryRow(
		`SELECT COUNT(*) FROM pancake_private_reply_sent
		 WHERE tenant_id = $1 AND sender_id = 'user-fail'`,
		tenantID,
	).Scan(&rows)
	if rows != 1 {
		t.Errorf("DB row count after retry = %d; want 1", rows)
	}
}

// Sanity: unit lines keep the imports alive even if the Testfile variant changes.
var _ = time.Second
