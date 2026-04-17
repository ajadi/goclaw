package pancake

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
)

// TestPrivateReplyRename_BehaviorPreserved is a characterization test that pins
// the one-time DM dedup behavior across the Phase 1 mechanical rename
// (first_inbox → private_reply). Same behavior, new names.
func TestPrivateReplyRename_BehaviorPreserved(t *testing.T) {
	cfg := pancakeInstanceConfig{}
	cfg.Features.PrivateReply = true
	cfg.PrivateReplyMessage = "Hi {name}"
	ch, transport := newChannelWithMultiCapture(t, cfg)

	outMsg := bus.OutboundMessage{
		ChatID:  "conv-1",
		Content: "public reply",
		Metadata: map[string]string{
			"pancake_mode":        "comment",
			"sender_id":           "user-1",
			"reply_to_comment_id": "comment-1",
		},
	}

	// First send: public reply + private_reply (DM).
	if err := ch.Send(context.Background(), outMsg); err != nil {
		t.Fatalf("first Send: %v", err)
	}

	// Second send: same sender → DM must be deduped (no second private_reply).
	outMsg.ChatID = "conv-2"
	outMsg.Metadata["reply_to_comment_id"] = "comment-2"
	if err := ch.Send(context.Background(), outMsg); err != nil {
		t.Fatalf("second Send: %v", err)
	}

	transport.mu.Lock()
	defer transport.mu.Unlock()

	var privateReplyCount int
	var privateReplyBody string
	for _, body := range transport.bodies {
		var p map[string]any
		if err := json.Unmarshal(body, &p); err != nil {
			continue
		}
		if p["action"] == "private_reply" {
			privateReplyCount++
			if msg, _ := p["message"].(string); msg != "" {
				privateReplyBody = msg
			}
		}
	}

	if privateReplyCount != 1 {
		t.Errorf("expected exactly 1 private_reply call (dedup per sender), got %d", privateReplyCount)
	}
	// P3 introduces template rendering. P1 pins literal passthrough: "{name}" stays as-is.
	if privateReplyBody != "Hi {name}" {
		t.Errorf("private_reply body = %q, want %q (no template processing at P1)",
			privateReplyBody, "Hi {name}")
	}
}
