package pancake

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
)

// handleCommentEvent processes a Pancake COMMENT webhook event.
// Mirrors the inbox handler pattern with additional comment-specific guards.
func (ch *Channel) handleCommentEvent(data MessagingData) {
	mode := resolvePrivateReplyMode(&ch.config)
	privateReplyOnly := mode == "standalone" && ch.config.Features.PrivateReply

	// Feature gate — exit if NOTHING to do. Standalone mode keeps the handler
	// alive even when comment_reply is off, as long as private_reply is on.
	if !ch.config.Features.CommentReply && !ch.config.Features.AutoReact && !privateReplyOnly {
		ch.commentReplyDisabledOnce.Do(func() {
			slog.Info("pancake: comment ignored because comment_reply, auto_react, and private_reply are all disabled",
				"page_id", ch.pageID,
				"channel_name", ch.Name(),
				"hint", "enable config.features.comment_reply, auto_react, or private_reply")
		})
		return
	}

	// Self-reply prevention: skip messages from the page itself.
	if data.Message.SenderID == ch.pageID {
		slog.Debug("pancake: skipping own page comment",
			"page_id", ch.pageID, "sender_id", data.Message.SenderID)
		return
	}

	// Skip assigned staff comments.
	if isAssignedStaff(data.AssigneeIDs, data.Message.SenderID) {
		slog.Debug("pancake: skipping assigned staff comment",
			"page_id", ch.pageID, "sender_id", data.Message.SenderID)
		return
	}

	if data.Message.SenderID == "" {
		slog.Warn("pancake: comment missing sender_id", "msg_id", data.Message.ID)
		return
	}

	if data.ConversationID == "" {
		slog.Warn("pancake: comment missing conversation_id", "msg_id", data.Message.ID)
		return
	}

	// Dedup by message ID (skip when empty to avoid shared slot).
	var dedupKey string
	if data.Message.ID != "" {
		dedupKey = fmt.Sprintf("comment:%s", data.Message.ID)
		if ch.isDup(dedupKey) {
			slog.Debug("pancake: duplicate comment skipped", "msg_id", data.Message.ID)
			return
		}
	}

	// Auto-react BEFORE keyword filter — fires on all valid non-duplicate comments.
	// Independent of comment_reply: reacts even if reply is disabled.
	if ch.config.Features.AutoReact && ch.platform == "facebook" && data.Message.ID != "" {
		select {
		case ch.reactSem <- struct{}{}:
			go ch.reactCommentAsync(data.ConversationID, data.Message.ID)
		default:
			slog.Debug("pancake: react semaphore full, dropping reaction",
				"page_id", ch.pageID, "comment_id", data.Message.ID)
		}
	}

	// Comment reply gate — independent of auto_react above. Standalone mode
	// proceeds even with comment_reply disabled (DM-only funnel).
	if !ch.config.Features.CommentReply && !privateReplyOnly {
		return
	}

	// Comment filter applies only to public reply content. Standalone mode
	// skips it because the DM is a template, not a filter-dependent response.
	if !privateReplyOnly && !ch.filterComment(data.Message.Content) {
		slog.Debug("pancake: comment filtered out",
			"page_id", ch.pageID, "msg_id", data.Message.ID)
		return
	}

	// Echo check before content enrichment.
	if ch.isRecentOutboundEcho(data.ConversationID, data.Message.Content) {
		slog.Debug("pancake: skipping comment outbound echo",
			"page_id", ch.pageID, "msg_id", data.Message.ID)
		return
	}

	// Build content — optionally enriched with post context.
	content := ch.buildCommentContent(data)

	metadata := map[string]string{
		"pancake_mode":        "comment",
		"conversation_type":   data.Type,
		"reply_to_comment_id": data.Message.ID,
		"sender_id":           data.Message.SenderID,
		"platform":            data.Platform,
		"conversation_id":     data.ConversationID,
		"message_id":          dedupKey,
		"display_name":        channels.SanitizeDisplayName(data.Message.SenderName),
		"page_name":           ch.pageName,
		"private_reply_mode":  mode,
	}
	if privateReplyOnly {
		metadata["private_reply_only"] = "true"
	}
	if data.PostID != "" {
		metadata["post_id"] = data.PostID
	}

	// Standalone fast-path (plan H6 fix): bypass the agent pipeline. The DM is a
	// template, not an LLM-generated response — running the pipeline would burn
	// tokens on a message that will be discarded in sendCommentReply. Publish a
	// synthetic outbound directly so dispatchOutbound → Send → sendCommentReply
	// → sendPrivateReply handles it without touching HandleMessage.
	if privateReplyOnly {
		_ = content // built for logging parity; pipeline skipped on purpose
		outbound := bus.OutboundMessage{
			Channel:  ch.Name(),
			ChatID:   data.ConversationID,
			Content:  "", // sendPrivateReply renders the template at send time
			Metadata: metadata,
		}
		if mb := ch.Bus(); mb != nil {
			mb.PublishOutbound(outbound)
		} else {
			slog.Warn("pancake: standalone fast-path skipped — bus nil",
				"page_id", ch.pageID, "sender_id", data.Message.SenderID)
		}
		return
	}

	// ChatID = ConversationID: Pancake groups COMMENT conversations per sender per post.
	ch.HandleMessage(
		data.Message.SenderID,
		data.ConversationID,
		content,
		nil,
		metadata,
		"direct",
	)

	slog.Debug("pancake: comment event published to bus",
		"page_id", ch.pageID,
		"conv_id", data.ConversationID,
		"sender_id", data.Message.SenderID,
		"platform", data.Platform,
	)
}

// buildCommentContent assembles the comment content, optionally enriched with post context.
// Uses display name only (no senderID in content — senderID stays in metadata).
func (ch *Channel) buildCommentContent(data MessagingData) string {
	commentText := stripHTML(data.Message.Content)
	senderName := channels.SanitizeDisplayName(data.Message.SenderName)
	senderPrefix := fmt.Sprintf("[From: %s]", senderName)

	if !ch.config.CommentReplyOptions.IncludePostContext || ch.postFetcher == nil {
		if commentText != "" {
			return senderPrefix + " " + commentText
		}
		return senderPrefix
	}

	var sb strings.Builder

	// Fetch post context best-effort — on failure, fall back to comment text only.
	if data.PostID != "" {
		post, err := ch.postFetcher.GetPost(ch.stopCtx, data.PostID)
		if err != nil {
			slog.Debug("pancake: post context fetch failed, using comment only",
				"page_id", ch.pageID, "post_id", data.PostID, "err", err)
		}
		if err == nil && post != nil && post.Message != "" {
			sb.WriteString("[Bai dang] ")
			sb.WriteString(post.Message)
			sb.WriteString("\n\n")
		}
	}

	sb.WriteString("[Comment moi] ")
	sb.WriteString(senderPrefix)
	if commentText != "" {
		sb.WriteString(" ")
		sb.WriteString(commentText)
	}

	return sb.String()
}

// reactCommentAsync likes the comment on Facebook asynchronously.
// Respects channel shutdown via stopCtx with 5s cap; releases the semaphore slot on exit.
func (ch *Channel) reactCommentAsync(conversationID, messageID string) {
	defer func() { <-ch.reactSem }()

	ctx, cancel := context.WithTimeout(ch.stopCtx, 5*time.Second)
	defer cancel()

	if err := ch.apiClient.ReactComment(ctx, conversationID, messageID); err != nil {
		slog.Warn("pancake: auto-react comment failed",
			"comment_id", messageID, "conv_id", conversationID, "page_id", ch.pageID, "err", err)
		return
	}
	slog.Debug("pancake: auto-reacted to comment",
		"comment_id", messageID, "page_id", ch.pageID)
}

// filterComment checks if the comment matches the configured filter.
// Returns true if the comment should be processed.
func (ch *Channel) filterComment(content string) bool {
	switch ch.config.CommentReplyOptions.Filter {
	case "keyword":
		if len(ch.config.CommentReplyOptions.Keywords) == 0 {
			// No keywords configured = block all (safe default).
			slog.Warn("pancake: keyword filter active but no keywords configured, blocking all comments",
				"page_id", ch.pageID)
			return false
		}
		lower := strings.ToLower(content)
		for _, kw := range ch.config.CommentReplyOptions.Keywords {
			if strings.Contains(lower, strings.ToLower(kw)) {
				return true
			}
		}
		return false
	default: // "all" or empty — process all comments
		return true
	}
}
