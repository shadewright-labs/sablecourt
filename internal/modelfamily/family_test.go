package modelfamily

import (
	"errors"
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
		{"chatgpt-4o-latest", Unknown},
		{"o2-mini", Unknown},
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
		{"prefix wins: local label on a gpt model", "gpt-6.2-astra", "local", Unknown, ReasonOverrideConflict},
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

func TestReasonOfForeignError(t *testing.T) {
	if r := ReasonOf(errors.New("other")); r != "" {
		t.Fatalf("ReasonOf(foreign) = %q", r)
	}
	if r := ReasonOf(nil); r != "" {
		t.Fatalf("ReasonOf(nil) = %q", r)
	}
}
