package pancake

import (
	"strings"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/i18n"
)

// defaultPrivateReplyTTL is the fallback TTL when PrivateReplyTTLDays is 0.
const defaultPrivateReplyTTL = 7 * 24 * time.Hour

// defaultPrivateReplyMessage returns the built-in DM fallback for the given
// locale. Resolves via internal/i18n (MsgPancakePrivateReplyDefault), which
// falls back to English when the locale catalog lacks the key.
func defaultPrivateReplyMessage(locale string) string {
	return i18n.T(locale, i18n.MsgPancakePrivateReplyDefault)
}

// resolvePrivateReplyTTL returns the configured TTL. Non-positive values fall
// back to the 7-day default (garbage-in-safe).
func resolvePrivateReplyTTL(cfg *pancakeInstanceConfig) time.Duration {
	if cfg == nil || cfg.PrivateReplyTTLDays <= 0 {
		return defaultPrivateReplyTTL
	}
	return time.Duration(cfg.PrivateReplyTTLDays) * 24 * time.Hour
}

// resolvePrivateReplyMode maps config values to a canonical mode string.
// Valid: "after_reply" (default), "standalone". Anything else — including the
// dropped "both" — falls back to "after_reply".
func resolvePrivateReplyMode(cfg *pancakeInstanceConfig) string {
	if cfg != nil && cfg.PrivateReplyMode == "standalone" {
		return "standalone"
	}
	return "after_reply"
}

// renderPrivateReplyMessage substitutes {{key}} placeholders with vars values.
// Pre-sanitizes values (strips "{{" and "}}") so a var value cannot inject
// another placeholder regardless of Go map iteration order.
// Empty tmpl uses the locale-appropriate default via i18n (English fallback
// when catalog missing the key).
// Unknown placeholders are left as-is (seller-friendly, not a hard error).
func renderPrivateReplyMessage(tmpl, locale string, vars map[string]string) string {
	if tmpl == "" {
		tmpl = defaultPrivateReplyMessage(locale)
	}
	out := tmpl
	for k, v := range vars {
		safe := strings.ReplaceAll(v, "{{", "")
		safe = strings.ReplaceAll(safe, "}}", "")
		out = strings.ReplaceAll(out, "{{"+k+"}}", safe)
	}
	return out
}

// filterPrivateReply returns true when the post passes the configured scope
// filters. Nil opts = allow all. Deny beats allow. Empty postID = deny
// (scope filtering requires a known post).
func filterPrivateReply(opts *PrivateReplyOptions, postID string) bool {
	if opts == nil || (len(opts.AllowPostIDs) == 0 && len(opts.DenyPostIDs) == 0) {
		return true
	}
	if postID == "" {
		return false
	}
	if containsPostID(opts.DenyPostIDs, postID) {
		return false
	}
	if len(opts.AllowPostIDs) > 0 && !containsPostID(opts.AllowPostIDs, postID) {
		return false
	}
	return true
}

// containsPostID reports whether target matches any entry in list, trimming
// whitespace on each entry so operators can paste comma-separated UI values.
func containsPostID(list []string, target string) bool {
	for _, item := range list {
		if strings.TrimSpace(item) == target {
			return true
		}
	}
	return false
}
