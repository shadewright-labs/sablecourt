package modelfamily

import (
	"errors"
	"strings"
	"testing"
)

func TestBuiltin(t *testing.T) {
	cases := []struct {
		model string
		want  Family
	}{
		{"claude-opus-5-5", Anthropic},
		{"claude-sonnet-5-5", Anthropic},
		{"Claude-Opus-5-5", Anthropic},
		{" claude-haiku-4 ", Anthropic},
		{"opus", Anthropic},
		{"sonnet", Anthropic},
		{"haiku", Anthropic},
		{"gpt-6.2-astra", OpenAI},
		{"codex-mini", OpenAI},
		{"o1-preview", OpenAI},
		{"o3-mini", OpenAI},
		{"o4-mini", OpenAI},
		{"gemini-3.8-flash-high", Google},
		{"GEMINI-pro", Google},
		// Neighbours of the prefixes and aliases stay unknown.
		{"", Unknown},
		{"claude", Unknown},
		{"anthropic-claude-3", Unknown},
		{"opus-local", Unknown},
		{"sonnets", Unknown},
		{"gpt", Unknown},
		{"chatgpt-4o-latest", OpenAI},
		{"CHATGPT-4o", OpenAI},
		{"o1", OpenAI},
		{"o3", OpenAI},
		{"o1x", Unknown},
		{"o10", Unknown},
		{"o3mini", Unknown},
		{"o4.1", Unknown},
		{"o2-mini", Unknown},
		{"claude-", Unknown},
		{"gpt-", Unknown},
		{"chatgpt", Unknown},
		{"chatgpt-", Unknown},
		// Vendor paths count only where the path names the same family.
		{"anthropic/claude-opus-5-5", Anthropic},
		{"openai/gpt-6.2-astra", OpenAI},
		{"openai/o3", OpenAI},
		{"openai/o7-mini", OpenAI},
		{"openai/chatgpt-4o-latest", OpenAI},
		{"google/gemini-3.8-pro", Google},
		{"models/gemini-3.8-pro", Google},
		{"Anthropic/Claude-Opus", Anthropic},
		{"openai/o1x", Unknown},
		{"openai/o10", Unknown},
		{"openai/codex-mini", Unknown},
		{"openai/claude-opus", Unknown},
		{"anthropic/gpt-6", Unknown},
		{"google/claude-opus", Unknown},
		{"models/claude-opus", Unknown},
		{"anthropic/opus", Unknown},
		{"anthropic/claude-", Unknown},
		{"a/anthropic/claude-opus", Unknown},
		{"anthropic/claude-opus/x", Unknown},
		{"/claude-opus", Unknown},
		{"o5", Unknown},
		{"gemini", Unknown},
		{"llama-3.3-70b", Unknown},
		{"my-gateway/claude-opus", Unknown},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			if got := Builtin(tc.model); got != tc.want {
				t.Fatalf("Builtin(%q) = %q, want %q", tc.model, got, tc.want)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name       string
		model      string
		override   string
		want       Family
		wantReason Reason
	}{
		{"prefix without override", "claude-opus-5-5", "", Anthropic, ""},
		{"prefix with agreeing override", "gpt-6.2-astra", "openai", OpenAI, ""},
		{"agreeing override in another case", "gemini-pro", " Google ", Google, ""},
		{"alias with agreeing override", "opus", "anthropic", Anthropic, ""},
		{"unknown name with override", "llama-3.3-70b", "meta", Family("meta"), ""},
		{"gateway name labelled as a built-in family", "my-gateway/claude-opus", "anthropic", Anthropic, ""},
		{"prefix wins: override disagrees", "claude-opus-5-5", "openai", Unknown, ReasonOverrideConflict},
		{"prefix wins: other known label on a gpt model", "gpt-6.2-astra", "meta", Unknown, ReasonOverrideConflict},
		{"vendor path wins: override disagrees", "anthropic/claude-opus-5-5", "openai", Unknown, ReasonOverrideConflict},
		{"vendor path wins: models path relabelled", "models/gemini-pro", "anthropic", Unknown, ReasonOverrideConflict},
		{"chatgpt relabelled", "chatgpt-4o-latest", "anthropic", Unknown, ReasonOverrideConflict},
		{"other vendor path with override", "openai/claude-opus", "anthropic", Anthropic, ""},
		{"override unknown", "llama-3.3-70b", "unknown", Unknown, ReasonOverrideInvalid},
		{"override Unknown on a prefix model", "claude-opus-5-5", " Unknown ", Unknown, ReasonOverrideInvalid},
		{"override outside the known set", "llama-3.3-70b", "local", Unknown, ReasonOverrideInvalid},
		{"override outside the known set on a prefix model", "gpt-6.2-astra", "local", Unknown, ReasonOverrideInvalid},
		{"prefix wins: alias relabelled", "haiku", "google", Unknown, ReasonOverrideConflict},
		{"unknown name, no override", "llama-3.3-70b", "", Unknown, ReasonUnknown},
		{"unknown name, blank override", "llama-3.3-70b", "   ", Unknown, ReasonUnknown},
		{"empty model, no override", "", "", Unknown, ReasonUnknown},
		{"empty model with override", "", "meta", Unknown, ReasonUnknown},
		{"override with a space", "llama-3", "my family", Unknown, ReasonOverrideInvalid},
		{"override with a slash", "llama-3", "meta/llama", Unknown, ReasonOverrideInvalid},
		{"override starting with a digit", "llama-3", "3meta", Unknown, ReasonOverrideInvalid},
		{"override too long", "llama-3", "a123456789012345678901234567890123", Unknown, ReasonOverrideInvalid},
		{"invalid override on a prefix model", "claude-opus-5-5", "anthropic!", Unknown, ReasonOverrideInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Classify(tc.model, tc.override)
			if tc.wantReason == "" {
				if err != nil || got != tc.want {
					t.Fatalf("Classify(%q, %q) = %q, %v; want %q, nil", tc.model, tc.override, got, err, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("Classify(%q, %q) = %q, nil; want refusal %s", tc.model, tc.override, got, tc.wantReason)
			}
			if got != Unknown {
				t.Fatalf("Classify(%q, %q) returned family %q with a refusal", tc.model, tc.override, got)
			}
			if r := ReasonOf(err); r != tc.wantReason {
				t.Fatalf("Classify(%q, %q) reason = %q (%v), want %q", tc.model, tc.override, r, err, tc.wantReason)
			}
		})
	}
}

// TestBuiltinNeighbours generates neighbours of every built-in rule: a
// character before a rule, or a character after a rule other than what its
// boundary allows, must leave the name unknown.
func TestBuiltinNeighbours(t *testing.T) {
	const alphabet = "abcxyz019-._:/@ "
	bases := []struct {
		prefix string
		family Family
		exact  bool // the rule matches only exactly or followed by '-'
	}{
		{"claude-", Anthropic, false},
		{"gpt-", OpenAI, false},
		{"chatgpt-", OpenAI, false},
		{"codex-", OpenAI, false},
		{"o1", OpenAI, true},
		{"o3", OpenAI, true},
		{"o4", OpenAI, true},
		{"gemini-", Google, false},
		{"anthropic/claude-", Anthropic, false},
		{"openai/gpt-", OpenAI, false},
		{"openai/chatgpt-", OpenAI, false},
		{"openai/o2", OpenAI, true},
		{"openai/o9", OpenAI, true},
		{"google/gemini-", Google, false},
		{"models/gemini-", Google, false},
	}
	for _, b := range bases {
		for _, c := range alphabet {
			if c == ' ' {
				continue // surrounding space is trimmed
			}
			before := string(c) + b.prefix + "x"
			if b.exact {
				before = string(c) + b.prefix
			}
			if got := Builtin(before); got != Unknown {
				t.Errorf("Builtin(%q) = %q, want unknown", before, got)
			}
		}
		for _, c := range alphabet {
			after := b.prefix + string(c)
			want := Unknown
			switch {
			case b.exact && c == ' ':
				want = b.family // trimmed back to the exact name
			case b.exact && c == '-':
				want = Unknown // "o1-" has nothing after the boundary
			case !b.exact && c != '/' && c != ' ':
				want = b.family
			}
			if got := Builtin(after); got != want {
				t.Errorf("Builtin(%q) = %q, want %q", after, got, want)
			}
			if b.exact {
				name := b.prefix + "-" + string(c)
				want := b.family
				if c == '/' || c == ' ' {
					want = Unknown
				}
				if got := Builtin(name); got != want {
					t.Errorf("Builtin(%q) = %q, want %q", name, got, want)
				}
			}
		}
	}
	for _, alias := range []string{"opus", "sonnet", "haiku"} {
		for _, c := range alphabet {
			for _, name := range []string{string(c) + alias, alias + string(c)} {
				if got := Builtin(name); got != Unknown && strings.TrimSpace(name) != alias {
					t.Errorf("Builtin(%q) = %q, want unknown", name, got)
				}
			}
		}
	}
}

func TestKnownOverrideLabels(t *testing.T) {
	for _, label := range KnownFamilies() {
		if label == Unknown || label == "unknown" {
			t.Fatalf("known set holds %q", label)
		}
		if got, err := NormalizeOverride(" " + strings.ToUpper(string(label)) + " "); err != nil || got != string(label) {
			t.Fatalf("NormalizeOverride(%q) = %q, %v", label, got, err)
		}
	}
}

func TestReasonOfForeignError(t *testing.T) {
	if r := ReasonOf(errors.New("other")); r != "" {
		t.Fatalf("ReasonOf(foreign) = %q", r)
	}
	if r := ReasonOf(nil); r != "" {
		t.Fatalf("ReasonOf(nil) = %q", r)
	}
}
