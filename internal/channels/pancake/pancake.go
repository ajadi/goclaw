package pancake

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/metrics"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Compile-time interface assertions.
var (
	_ channels.Channel           = (*Channel)(nil)
	_ channels.WebhookChannel    = (*Channel)(nil)
	_ channels.BlockReplyChannel = (*Channel)(nil)
)

const (
	dedupTTL        = 24 * time.Hour
	dedupCleanEvery = 5 * time.Minute
	outboundEchoTTL = 45 * time.Second
)

// Channel implements channels.Channel and channels.WebhookChannel for Pancake (pages.fm).
// One channel instance = one Pancake page, which may serve multiple platforms (FB, Zalo, IG, etc.)
type Channel struct {
	*channels.BaseChannel
	config        pancakeInstanceConfig
	apiClient     *APIClient
	pageID        string
	webhookPageID string // native platform ID used in webhook event.page_id (may differ from Pancake internal pageID)
	pageName      string // resolved from Pancake page metadata at Start()
	platform      string // resolved from Pancake page metadata at Start()
	webhookSecret string // optional HMAC-SHA256 secret for webhook verification

	// dedup prevents processing duplicate webhook deliveries.
	dedup sync.Map // eventKey(string) → time.Time

	// recentOutbound suppresses short-lived webhook echoes of our own text replies.
	recentOutbound sync.Map // conversationID + "\x00" + normalized content → time.Time

	// privateReplyStore persists the one-time private-reply DM dedup marker per
	// (tenant, page, sender). Replaces the in-memory sync.Map so dedup survives
	// channel restarts. Nil = feature disabled (FactoryWithStores wiring missing).
	privateReplyStore store.PancakePrivateReplyStore

	// postFetcher fetches and caches page post content for comment context enrichment.
	postFetcher *PostFetcher

	// commentReplyDisabledOnce prevents repeated info logs when COMMENT webhooks
	// arrive but the feature is disabled in channel config.
	commentReplyDisabledOnce sync.Once

	// reactSem bounds concurrent Facebook comment-like calls (cap 10).
	reactSem chan struct{}

	stopCh  chan struct{}
	stopCtx context.Context
	stopFn  context.CancelFunc
}

// New creates a Pancake Channel from parsed credentials and config.
func New(cfg pancakeInstanceConfig, creds pancakeCreds,
	msgBus *bus.MessageBus, _ store.PairingStore) (*Channel, error) {

	if creds.APIKey == "" {
		return nil, fmt.Errorf("pancake: api_key is required")
	}
	if creds.PageAccessToken == "" {
		return nil, fmt.Errorf("pancake: page_access_token is required")
	}
	if cfg.PageID == "" {
		return nil, fmt.Errorf("pancake: page_id is required")
	}

	base := channels.NewBaseChannel(channels.TypePancake, msgBus, cfg.AllowFrom)
	stopCtx, stopFn := context.WithCancel(context.Background())

	apiClient := NewAPIClient(creds.APIKey, creds.PageAccessToken, cfg.PageID)
	ch := &Channel{
		BaseChannel:   base,
		config:        cfg,
		apiClient:     apiClient,
		pageID:        cfg.PageID,
		webhookPageID: cfg.WebhookPageID,
		platform:      cfg.Platform,
		webhookSecret: creds.WebhookSecret,
		postFetcher:   NewPostFetcher(apiClient, cfg.PostContextCacheTTL),
		reactSem:      make(chan struct{}, 10),
		stopCh:        make(chan struct{}),
		stopCtx:       stopCtx,
		stopFn:        stopFn,
	}
	ch.postFetcher.stopCtx = stopCtx

	return ch, nil
}

// Factory creates a Pancake Channel from DB instance data.
// Implements channels.ChannelFactory.
//
// Factory leaves privateReplyStore nil — the PrivateReply feature is then a
// no-op (channel still starts, comments still flow, just no dedup/DM). Use
// FactoryWithStores to enable the feature in production.
func Factory(name string, creds json.RawMessage, cfg json.RawMessage,
	msgBus *bus.MessageBus, pairingSvc store.PairingStore) (channels.Channel, error) {
	return buildFactory(nil)(name, creds, cfg, msgBus, pairingSvc)
}

// FactoryWithStores returns a ChannelFactory bound to the provided stores.
// Wires the PancakePrivateReplyStore so the PrivateReply feature can persist
// dedup across channel restarts. Follows the established FactoryWithStores*
// pattern used by telegram, discord, whatsapp in cmd/gateway.go.
func FactoryWithStores(privateReply store.PancakePrivateReplyStore) channels.ChannelFactory {
	return buildFactory(privateReply)
}

// buildFactory is the shared Factory constructor body parameterised by the
// optional PrivateReply store.
func buildFactory(privateReply store.PancakePrivateReplyStore) channels.ChannelFactory {
	return func(name string, creds json.RawMessage, cfg json.RawMessage,
		msgBus *bus.MessageBus, pairingSvc store.PairingStore) (channels.Channel, error) {

		var c pancakeCreds
		if err := json.Unmarshal(creds, &c); err != nil {
			return nil, fmt.Errorf("pancake: decode credentials: %w", err)
		}

		var ic pancakeInstanceConfig
		if len(cfg) > 0 {
			if err := json.Unmarshal(cfg, &ic); err != nil {
				return nil, fmt.Errorf("pancake: decode config: %w", err)
			}
		}

		ch, err := New(ic, c, msgBus, pairingSvc)
		if err != nil {
			return nil, err
		}
		ch.privateReplyStore = privateReply
		ch.SetName(name)
		return ch, nil
	}
}

// Start connects the channel: verifies token, resolves platform, registers webhook.
func (ch *Channel) Start(ctx context.Context) error {
	ch.MarkStarting("connecting to Pancake page")

	if err := ch.apiClient.VerifyToken(ctx); err != nil {
		ch.MarkFailed("token invalid", err.Error(), channels.ChannelFailureKindAuth, false)
		return err
	}

	// Resolve platform and page name from page metadata (best-effort — don't fail on this).
	if ch.platform == "" || ch.pageName == "" {
		if page, err := ch.apiClient.GetPage(ctx); err != nil {
			slog.Warn("pancake: could not resolve platform from page metadata", "page_id", ch.pageID, "err", err)
		} else {
			if page.Platform != "" {
				slog.Debug("pancake: platform auto-detected; set platform explicitly in config to avoid startup API call",
					"page_id", ch.pageID, "platform", page.Platform)
				ch.platform = page.Platform
			}
			if page.Name != "" {
				ch.pageName = page.Name
			}
		}
	}

	if ch.webhookSecret == "" {
		slog.Warn("security.pancake_webhook_no_secret",
			"page_id", ch.pageID,
			"note", "webhook_secret not configured; incoming webhook requests will not be authenticated")
	}

	// Without HMAC, any actor reaching the webhook endpoint can trigger Pancake API calls.
	if ch.config.Features.AutoReact && ch.webhookSecret == "" {
		slog.Warn("security.pancake_auto_react_without_hmac: auto_react is enabled but "+
			"webhook_secret is not set; configure webhook_secret to prevent "+
			"unauthenticated like-comment triggers",
			"page_id", ch.pageID)
	}

	globalRouter.register(ch)
	ch.MarkHealthy("connected to page " + ch.pageID)
	ch.SetRunning(true)

	// Background goroutine: evict stale dedup entries to prevent memory growth.
	go ch.runDedupCleaner()

	slog.Info("pancake channel started",
		"page_id", ch.pageID,
		"platform", ch.platform,
		"name", ch.Name())
	return nil
}

// Stop gracefully shuts down the channel.
func (ch *Channel) Stop(_ context.Context) error {
	globalRouter.unregister(ch.pageID, ch.webhookPageID)
	ch.stopFn()
	close(ch.stopCh)
	ch.SetRunning(false)
	ch.MarkStopped("stopped")
	slog.Info("pancake channel stopped", "page_id", ch.pageID, "name", ch.Name())
	return nil
}

// Send delivers an outbound message via Pancake API.
// Routes to sendCommentReply or sendInboxReply based on metadata["pancake_mode"].
//
// Injects the channel's tenant into ctx so downstream store calls (notably the
// PancakePrivateReply dedup store) see a non-nil tenant. Without this, the
// store returns ErrMissingTenantID and silently skips the DM in production —
// unit fakes masked this because they bypass tenant checks.
func (ch *Channel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if tid := ch.TenantID(); tid != uuid.Nil && store.TenantIDFromContext(ctx) == uuid.Nil {
		ctx = store.WithTenantID(ctx, tid)
	}

	slog.Debug("pancake: Send called",
		"page_id", ch.pageID,
		"chat_id", msg.ChatID,
		"content_len", len(msg.Content),
		"platform", ch.platform,
		"channel_name", ch.Name())

	if msg.ChatID == "" {
		return fmt.Errorf("pancake: chat_id (conversation_id) is required for outbound message")
	}

	// NO_REPLY / suppressed-error path: empty content with no media means the
	// caller wants downstream cleanup only. Pancake API rejects empty payloads,
	// so short-circuit before dispatch.
	if msg.Content == "" && len(msg.Media) == 0 {
		return nil
	}

	switch msg.Metadata["pancake_mode"] {
	case "comment":
		return ch.sendCommentReply(ctx, msg)
	default: // "inbox" or unset — existing behavior
		return ch.sendInboxReply(ctx, msg)
	}
}

// sendInboxReply handles outbound inbox messages (existing logic extracted from Send).
func (ch *Channel) sendInboxReply(ctx context.Context, msg bus.OutboundMessage) error {
	conversationID := msg.ChatID
	text := FormatOutbound(msg.Content, ch.platform)

	// Handle media attachments.
	attachmentIDs, err := ch.handleMediaAttachments(ctx, msg)
	if err != nil {
		slog.Warn("pancake: media upload failed, sending text only",
			"page_id", ch.pageID, "err", err)
	}

	// Deliver uploaded media first, then follow with text chunks if needed.
	if len(attachmentIDs) > 0 {
		if err := ch.apiClient.SendAttachmentMessage(ctx, conversationID, attachmentIDs); err != nil {
			ch.handleAPIError(err)
			return err
		}
		if text == "" {
			return nil
		}
	}

	// Text-only: split into platform-appropriate chunks.
	// Store echo fingerprints BEFORE sending so that webhook echoes arriving
	// while the HTTP round-trip is in flight are already recognized as self-sent.
	parts := splitMessage(text, ch.maxMessageLength())
	for _, part := range parts {
		ch.rememberOutboundEcho(conversationID, part)
	}
	for _, part := range parts {
		if err := ch.apiClient.SendMessage(ctx, conversationID, part); err != nil {
			ch.handleAPIError(err)
			ch.forgetOutboundEcho(conversationID, part)
			return err
		}
	}
	return nil
}

// sendCommentReply dispatches to the public-reply path and/or the private
// reply DM depending on the private_reply_mode set by handleCommentEvent.
//
// Modes (metadata "private_reply_mode" or "private_reply_only=true"):
//   - after_reply (default): public ReplyComment → DM via sendPrivateReply
//   - standalone: skip public reply, DM only (reply_to_comment_id may be empty)
//
// The reply_to_comment_id guard is hoisted inside the public-reply branch so
// standalone mode doesn't fail it (see plan C3 fix).
func (ch *Channel) sendCommentReply(ctx context.Context, msg bus.OutboundMessage) error {
	// Bound API calls: ReplyComment + PrivateReply can hang if Pancake is slow.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	conversationID := msg.ChatID
	privateReplyOnly := msg.Metadata["private_reply_only"] == "true"

	// Public reply path: requires reply_to_comment_id. Skipped entirely in
	// standalone mode so the guard below does not fire.
	if !privateReplyOnly {
		commentID := msg.Metadata["reply_to_comment_id"]
		if commentID == "" {
			return fmt.Errorf("pancake: reply_to_comment_id missing in outbound metadata for comment reply")
		}

		text := FormatOutbound(msg.Content, ch.platform)
		parts := splitMessage(text, ch.maxMessageLength())
		for _, part := range parts {
			ch.rememberOutboundEcho(conversationID, part)
		}
		for _, part := range parts {
			if err := ch.apiClient.ReplyComment(ctx, conversationID, commentID, part); err != nil {
				ch.handleAPIError(err)
				ch.forgetOutboundEcho(conversationID, part)
				return err
			}
		}
	}

	// Private reply: one-time DM (best-effort). Runs for every mode when the
	// feature is enabled — dedup lives in sendPrivateReply.
	if ch.config.Features.PrivateReply {
		senderID := msg.Metadata["sender_id"]
		if senderID != "" {
			ch.sendPrivateReply(
				ctx,
				senderID,
				conversationID,
				msg.Metadata["post_id"],
				msg.Metadata["display_name"],
			)
		}
	}

	return nil
}

// sendPrivateReply sends the one-time DM to a commenter (best-effort).
// Idempotent via DB-backed dedup store; survives channel restarts.
//
// Fail modes:
//   - Feature disabled / store unwired → silent no-op (by design)
//   - Scope filter reject → silent no-op
//   - Store WasSent=true → silent no-op (already DM'd within TTL)
//   - Store WasSent error → fail-closed (skip send; avoid duplicate DM spam)
//   - PrivateReply API error → log warn, do NOT mark sent (allow retry on next comment)
//   - MarkSent error after successful DM → log warn, non-fatal (may re-DM next time)
func (ch *Channel) sendPrivateReply(ctx context.Context, senderID, conversationID, postID, commenterName string) {
	if !ch.config.Features.PrivateReply {
		metrics.RecordPancakePrivateReply(ch.pageID, metrics.ResultSkipped, metrics.ReasonFeatureOff)
		return
	}
	if ch.privateReplyStore == nil {
		slog.Debug("pancake: private_reply store not wired, feature inactive",
			"page_id", ch.pageID, "sender_id", senderID)
		metrics.RecordPancakePrivateReply(ch.pageID, metrics.ResultSkipped, metrics.ReasonStoreUnwired)
		return
	}
	if !filterPrivateReply(ch.config.PrivateReplyOptions, postID) {
		slog.Debug("pancake: private_reply filtered by scope options",
			"page_id", ch.pageID, "post_id", postID, "sender_id", senderID)
		metrics.RecordPancakePrivateReply(ch.pageID, metrics.ResultSkipped, metrics.ReasonScopeFilter)
		return
	}

	ttl := resolvePrivateReplyTTL(&ch.config)

	// Atomic claim: INSERT ... ON CONFLICT DO UPDATE-WHERE-stale. Eliminates
	// the WasSent→Send→MarkSent TOCTOU where two concurrent comments from the
	// same sender would both pass the check and both fire the API.
	claimed, err := ch.privateReplyStore.TryClaim(ctx, ch.pageID, senderID, ttl)
	if err != nil {
		slog.Warn("pancake: private_reply claim failed — skipping send to avoid duplicate",
			"page_id", ch.pageID, "sender_id", senderID, "err", err)
		metrics.RecordPancakePrivateReply(ch.pageID, metrics.ResultSkipped, metrics.ReasonDedupError)
		return
	}
	if !claimed {
		slog.Debug("pancake: private_reply skipped (already sent within TTL)",
			"page_id", ch.pageID, "sender_id", senderID, "ttl", ttl)
		metrics.RecordPancakePrivateReply(ch.pageID, metrics.ResultSkipped, metrics.ReasonDedupHit)
		return
	}

	// Best-effort post title fetch for template var.
	postTitle := ""
	if postID != "" && ch.postFetcher != nil {
		if post, perr := ch.postFetcher.GetPost(ctx, postID); perr == nil && post != nil {
			postTitle = post.Message
		}
	}

	vars := map[string]string{
		"commenter_name": commenterName,
		"post_title":     postTitle,
	}
	locale := store.LocaleFromContext(ctx)
	rendered := renderPrivateReplyMessage(ch.config.PrivateReplyMessage, locale, vars)

	if err := ch.apiClient.PrivateReply(ctx, conversationID, rendered); err != nil {
		// Release the claim so next comment can retry (FB 7-day window, 403, etc.).
		if unclaimErr := ch.privateReplyStore.Unclaim(ctx, ch.pageID, senderID); unclaimErr != nil {
			slog.Warn("pancake: private_reply unclaim after API error failed (will re-dedup for one TTL)",
				"page_id", ch.pageID, "sender_id", senderID, "err", unclaimErr)
		}
		slog.Warn("pancake: private_reply send failed",
			"page_id", ch.pageID, "sender_id", senderID, "conv_id", conversationID, "err", err)
		metrics.RecordPancakePrivateReply(ch.pageID, metrics.ResultFailed, metrics.ReasonAPIError)
		return
	}

	metrics.RecordPancakePrivateReply(ch.pageID, metrics.ResultSent, metrics.ReasonNone)
	slog.Debug("pancake: private_reply sent",
		"page_id", ch.pageID, "sender_id", senderID, "conv_id", conversationID)
}

// BlockReplyEnabled returns the per-channel block_reply override (nil = inherit gateway default).
func (ch *Channel) BlockReplyEnabled() *bool { return ch.config.BlockReply }

// SetHTTPClientForTest swaps the underlying Pancake API client's HTTP client.
// Test-only hook — allows integration tests to inject an httptest.Server or a
// capturing RoundTripper without exposing the private apiClient field.
func (ch *Channel) SetHTTPClientForTest(client *http.Client) {
	if client != nil {
		ch.apiClient.httpClient = client
	}
}

// WebhookHandler returns the shared webhook path and global router as handler.
// Only the first pancake instance mounts the route; others return ("", nil).
func (ch *Channel) WebhookHandler() (string, http.Handler) {
	return globalRouter.webhookRoute()
}

// handleAPIError maps Pancake API errors to channel health states.
func (ch *Channel) handleAPIError(err error) {
	if err == nil {
		return
	}
	switch {
	case isAuthError(err):
		ch.MarkFailed("token expired or invalid", err.Error(), channels.ChannelFailureKindAuth, false)
	case isRateLimitError(err):
		ch.MarkDegraded("rate limited", err.Error(), channels.ChannelFailureKindNetwork, true)
	default:
		ch.MarkDegraded("api error", err.Error(), channels.ChannelFailureKindUnknown, true)
	}
}

// maxMessageLength returns the platform-specific character limit.
func (ch *Channel) maxMessageLength() int {
	switch ch.platform {
	case "tiktok":
		return 500
	case "instagram":
		return 1000
	case "facebook", "zalo":
		return 2000
	case "whatsapp":
		return 4096
	case "line":
		return 5000
	default:
		return 2000
	}
}

