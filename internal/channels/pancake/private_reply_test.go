package pancake

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/i18n"
)

func TestResolvePrivateReplyTTL(t *testing.T) {
	tests := []struct {
		name string
		days int
		want time.Duration
	}{
		{"zero defaults to 7d", 0, 7 * 24 * time.Hour},
		{"custom 3 days", 3, 3 * 24 * time.Hour},
		{"negative sanitized to 7d", -4, 7 * 24 * time.Hour},
		{"custom 1 day", 1, 24 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &pancakeInstanceConfig{PrivateReplyTTLDays: tc.days}
			if got := resolvePrivateReplyTTL(cfg); got != tc.want {
				t.Errorf("resolvePrivateReplyTTL(%d) = %v; want %v", tc.days, got, tc.want)
			}
		})
	}
	if got := resolvePrivateReplyTTL(nil); got != 7*24*time.Hour {
		t.Errorf("nil cfg → %v; want 7d", got)
	}
}

func TestResolvePrivateReplyMode(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", "after_reply"},
		{"standalone", "standalone"},
		{"after_reply", "after_reply"},
		{"both", "after_reply"},     // dropped mode falls back
		{"invalid", "after_reply"},  // unknown → default
		{"Standalone", "after_reply"}, // case-sensitive
	}
	for _, tc := range tests {
		cfg := &pancakeInstanceConfig{PrivateReplyMode: tc.in}
		if got := resolvePrivateReplyMode(cfg); got != tc.want {
			t.Errorf("resolvePrivateReplyMode(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
	if got := resolvePrivateReplyMode(nil); got != "after_reply" {
		t.Errorf("nil cfg → %q; want after_reply", got)
	}
}

func TestRenderPrivateReplyMessage(t *testing.T) {
	t.Run("empty template uses locale default (vi)", func(t *testing.T) {
		got := renderPrivateReplyMessage("", "vi", nil)
		want := i18n.T("vi", i18n.MsgPancakePrivateReplyDefault)
		if got != want {
			t.Errorf("vi empty tmpl → %q; want %q", got, want)
		}
		if !strings.Contains(got, "Cảm ơn") {
			t.Errorf("vi default should contain Vietnamese content; got %q", got)
		}
	})

	t.Run("empty template uses locale default (en)", func(t *testing.T) {
		got := renderPrivateReplyMessage("", "en", nil)
		if got != i18n.T("en", i18n.MsgPancakePrivateReplyDefault) {
			t.Errorf("en empty tmpl = %q", got)
		}
		if !strings.Contains(got, "Thanks") {
			t.Errorf("en default should contain English content; got %q", got)
		}
	})

	t.Run("empty template uses locale default (zh)", func(t *testing.T) {
		got := renderPrivateReplyMessage("", "zh", nil)
		if !strings.Contains(got, "感谢") {
			t.Errorf("zh default should contain Chinese content; got %q", got)
		}
	})

	t.Run("unknown locale falls back to English via i18n", func(t *testing.T) {
		got := renderPrivateReplyMessage("", "fr", nil)
		want := i18n.T("en", i18n.MsgPancakePrivateReplyDefault)
		if got != want {
			t.Errorf("fr locale → %q; want English fallback %q", got, want)
		}
	})

	t.Run("single var", func(t *testing.T) {
		got := renderPrivateReplyMessage("Hi {{commenter_name}}", "en", map[string]string{
			"commenter_name": "Tuan",
		})
		if got != "Hi Tuan" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("multiple vars", func(t *testing.T) {
		got := renderPrivateReplyMessage("Hi {{commenter_name}} from {{post_title}}", "en", map[string]string{
			"commenter_name": "Tuan",
			"post_title":     "Xmas sale",
		})
		if got != "Hi Tuan from Xmas sale" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("unknown placeholder left as-is", func(t *testing.T) {
		got := renderPrivateReplyMessage("Hi {{unknown}}", "en", map[string]string{
			"commenter_name": "Tuan",
		})
		if got != "Hi {{unknown}}" {
			t.Errorf("got %q; want placeholder preserved", got)
		}
	})

	t.Run("var value with braces does not inject new placeholder", func(t *testing.T) {
		got := renderPrivateReplyMessage("Hi {{commenter_name}} from {{post_title}}", "en", map[string]string{
			"commenter_name": "{{post_title}}",
			"post_title":     "Xmas",
		})
		if strings.Contains(got, "{{") || strings.Contains(got, "}}") {
			t.Errorf("render leaked braces: %q", got)
		}
	})

	t.Run("html-like content passes through", func(t *testing.T) {
		got := renderPrivateReplyMessage("Hi {{commenter_name}}", "en", map[string]string{
			"commenter_name": "<script>alert(1)</script>",
		})
		if got != "Hi <script>alert(1)</script>" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("missing vars render placeholder verbatim", func(t *testing.T) {
		got := renderPrivateReplyMessage("Hi {{commenter_name}} from {{post_title}}", "en", map[string]string{
			"commenter_name": "Tuan",
		})
		if got != "Hi Tuan from {{post_title}}" {
			t.Errorf("got %q", got)
		}
	})
}

func TestFilterPrivateReply(t *testing.T) {
	tests := []struct {
		name   string
		opts   *PrivateReplyOptions
		postID string
		want   bool
	}{
		{"nil opts → allow all", nil, "p1", true},
		{"empty opts → allow all", &PrivateReplyOptions{}, "p1", true},
		{"deny hit", &PrivateReplyOptions{DenyPostIDs: []string{"p1"}}, "p1", false},
		{"deny miss", &PrivateReplyOptions{DenyPostIDs: []string{"p2"}}, "p1", true},
		{"allow hit", &PrivateReplyOptions{AllowPostIDs: []string{"p1"}}, "p1", true},
		{"allow miss", &PrivateReplyOptions{AllowPostIDs: []string{"p1"}}, "p2", false},
		{"deny beats allow (same id)",
			&PrivateReplyOptions{AllowPostIDs: []string{"p1"}, DenyPostIDs: []string{"p1"}}, "p1", false},
		{"whitespace trimming",
			&PrivateReplyOptions{AllowPostIDs: []string{" p1 "}}, "p1", true},
		{"empty postID with filter → deny",
			&PrivateReplyOptions{AllowPostIDs: []string{"p1"}}, "", false},
		{"empty postID no filter → allow", nil, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := filterPrivateReply(tc.opts, tc.postID); got != tc.want {
				t.Errorf("got %v; want %v", got, tc.want)
			}
		})
	}
}

func TestPancakeConfig_PrivateReplyOptionsRoundtrip(t *testing.T) {
	cfg := pancakeInstanceConfig{
		PrivateReplyMessage: "Hi {{commenter_name}}",
		PrivateReplyMode:    "standalone",
		PrivateReplyTTLDays: 14,
		PrivateReplyOptions: &PrivateReplyOptions{
			AllowPostIDs: []string{"p1", "p2"},
			DenyPostIDs:  []string{"p9"},
		},
	}
	cfg.Features.PrivateReply = true

	buf, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round pancakeInstanceConfig
	if err := json.Unmarshal(buf, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if round.PrivateReplyMode != "standalone" {
		t.Errorf("mode = %q", round.PrivateReplyMode)
	}
	if round.PrivateReplyTTLDays != 14 {
		t.Errorf("ttl = %d", round.PrivateReplyTTLDays)
	}
	if round.PrivateReplyMessage != "Hi {{commenter_name}}" {
		t.Errorf("message = %q", round.PrivateReplyMessage)
	}
	if round.PrivateReplyOptions == nil ||
		len(round.PrivateReplyOptions.AllowPostIDs) != 2 ||
		len(round.PrivateReplyOptions.DenyPostIDs) != 1 {
		t.Errorf("options = %+v", round.PrivateReplyOptions)
	}
}

func TestPancakeConfig_PrivateReplyOptionsOmitempty(t *testing.T) {
	cfg := pancakeInstanceConfig{PageID: "p1"}
	buf, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw := string(buf)
	for _, key := range []string{
		"private_reply_options",
		"private_reply_message",
		"private_reply_mode",
		"private_reply_ttl_days",
	} {
		if strings.Contains(raw, key) {
			t.Errorf("expected %q omitted from empty config, got: %s", key, raw)
		}
	}
}

func TestPancakeConfig_PrivateReplyDefaults(t *testing.T) {
	// Absent keys roundtrip to zero values; resolvers apply the real defaults.
	raw := `{"page_id":"p1","features":{"private_reply":true}}`
	var cfg pancakeInstanceConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.PrivateReplyMode != "" || cfg.PrivateReplyTTLDays != 0 || cfg.PrivateReplyOptions != nil {
		t.Errorf("expected zero defaults, got mode=%q ttl=%d opts=%+v",
			cfg.PrivateReplyMode, cfg.PrivateReplyTTLDays, cfg.PrivateReplyOptions)
	}
	if resolvePrivateReplyMode(&cfg) != "after_reply" {
		t.Error("default mode should resolve to after_reply")
	}
	if resolvePrivateReplyTTL(&cfg) != 7*24*time.Hour {
		t.Error("default ttl should resolve to 7d")
	}
}
