package modelfamily

import (
	"strings"
	"testing"
)

func TestCheckVerifier(t *testing.T) {
	author := ChainEntry{Step: "edit", TaskID: "t1", Model: "claude-sonnet-5-5"}
	fixGPT := ChainEntry{Step: "edit", TaskID: "t3", Model: "gpt-6.2-astra"}
	fixGemini := ChainEntry{Step: "edit", TaskID: "t5", Model: "gemini-3.8-pro"}
	gptVerifier := Verifier{Step: "review", Model: "gpt-6.2-astra"}

	cases := []struct {
		name       string
		chain      []ChainEntry
		verifier   Verifier
		wantReason Reason
		wantText   string
	}{
		{"different family", []ChainEntry{author}, gptVerifier, "", ""},
		{"alias author, gemini verifier", []ChainEntry{{Step: "edit", Model: "opus"}}, Verifier{Step: "review", Model: "gemini-3.8-pro"}, "", ""},
		{"operator family on an unknown model", []ChainEntry{{Step: "edit", Model: "llama-3.3-70b", Family: "meta"}}, gptVerifier, "", ""},
		{"verifier with operator family", []ChainEntry{author}, Verifier{Step: "review", Model: "llama-3.3-70b", Family: "meta"}, "", ""},
		{"deterministic verifier with empty chain", nil, Verifier{Step: "check", Deterministic: true}, "", ""},
		{"deterministic verifier after same family", []ChainEntry{author}, Verifier{Step: "check", Deterministic: true, Model: "claude-opus-5-5"}, "", ""},

		{"same family as author", []ChainEntry{author}, Verifier{Step: "review", Model: "claude-opus-5-5"}, ReasonSameFamily, `step "edit"`},
		{"same family through an alias", []ChainEntry{author}, Verifier{Step: "review", Model: "sonnet"}, ReasonSameFamily, "anthropic"},
		{"agy running a claude model", []ChainEntry{author}, Verifier{Step: "review", Model: "claude-opus-5-5-high"}, ReasonSameFamily, "anthropic"},
		{"same family as a fix round", []ChainEntry{author, fixGPT}, gptVerifier, ReasonSameFamily, `task "t3"`},
		{"same family as a fast step", []ChainEntry{{Step: "triage", Model: "gemini-3.8-flash-high"}, author}, Verifier{Step: "review", Model: "gemini-3.8-pro"}, ReasonSameFamily, `step "triage"`},
		{"operator family equal to verifier's", []ChainEntry{{Step: "edit", Model: "my-gateway/model", Family: "openai"}}, gptVerifier, ReasonSameFamily, "openai"},
		{"no family left after three rounds", []ChainEntry{author, fixGPT, fixGemini}, Verifier{Step: "security", Model: "gemini-3.8-pro"}, ReasonSameFamily, ""},
		{"empty chain", nil, gptVerifier, ReasonChainEmpty, ""},
		{"empty chain slice", []ChainEntry{}, gptVerifier, ReasonChainEmpty, ""},
		{"verifier family unknown", []ChainEntry{author}, Verifier{Step: "review", Model: "llama-3.3-70b"}, ReasonUnknown, `verifier step "review"`},
		{"verifier without model", []ChainEntry{author}, Verifier{Step: "review"}, ReasonUnknown, ""},
		{"verifier override disagrees", []ChainEntry{author}, Verifier{Step: "review", Model: "gpt-6.2-astra", Family: "google"}, ReasonOverrideConflict, ""},
		{"verifier override invalid", []ChainEntry{author}, Verifier{Step: "review", Model: "llama-3", Family: "a b"}, ReasonOverrideInvalid, ""},
		{"chain entry unknown", []ChainEntry{author, {Step: "edit", TaskID: "t2", Model: "llama-3.3-70b"}}, gptVerifier, ReasonChainUnknown, `task "t2"`},
		{"chain entry without model", []ChainEntry{{Step: "edit", TaskID: "t1"}}, gptVerifier, ReasonChainUnknown, ""},
		{"chain entry override disagrees", []ChainEntry{{Step: "edit", Model: "claude-opus-5-5", Family: "meta"}}, gptVerifier, ReasonChainUnknown, ""},
		{"unknown verifier wins over empty chain", nil, Verifier{Step: "review", Model: "llama-3"}, ReasonUnknown, ""},
		{"unknown chain entry before a match", []ChainEntry{{Step: "triage", Model: "x-1"}, {Step: "edit", Model: "gpt-6"}}, gptVerifier, ReasonChainUnknown, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckVerifier(tc.chain, tc.verifier)
			if tc.wantReason == "" {
				if err != nil {
					t.Fatalf("CheckVerifier refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckVerifier allowed, want refusal %s", tc.wantReason)
			}
			if r := ReasonOf(err); r != tc.wantReason {
				t.Fatalf("reason = %q (%v), want %q", r, err, tc.wantReason)
			}
			if !strings.Contains(err.Error(), string(tc.wantReason)) {
				t.Fatalf("error text %q does not carry the reason %q", err, tc.wantReason)
			}
			if tc.wantText != "" && !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("error text %q lacks %q", err, tc.wantText)
			}
		})
	}
}

// The check never changes the chain it reads.
func TestCheckVerifierLeavesChainAlone(t *testing.T) {
	chain := []ChainEntry{{Step: "edit", Model: "claude-opus-5-5"}, {Step: "edit", Model: "llama-3", Family: "meta"}}
	before := append([]ChainEntry(nil), chain...)
	_ = CheckVerifier(chain, Verifier{Step: "review", Model: "gpt-6"})
	_ = CheckVerifier(chain, Verifier{Step: "review", Model: "claude-opus-5-5"})
	for i := range chain {
		if chain[i] != before[i] {
			t.Fatalf("entry %d changed: %+v", i, chain[i])
		}
	}
}
