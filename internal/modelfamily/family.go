// Package modelfamily is the daemon's one classifier of model families and
// the check that a workflow verifier step runs a family no model in its
// chain ran (ADR-0010, amendment of 2026-11-27).
//
// Built-in prefix rules always apply to the names they recognize. An
// operator may give a family from a closed set only to a model no rule
// recognizes, and a family that disagrees with a rule is refused. Any other
// name is unknown, and an unknown family refuses.
package modelfamily

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Family names a model family. The empty value is an unknown family.
type Family string

const (
	Unknown   Family = ""
	Anthropic Family = "anthropic"
	OpenAI    Family = "openai"
	Google    Family = "google"
)

// Reason is the recorded code of a refusal.
type Reason string

const (
	ReasonUnknown          Reason = "model_family_unknown"
	ReasonOverrideConflict Reason = "model_family_override_conflict"
	ReasonOverrideInvalid  Reason = "model_family_override_invalid"
	ReasonChainEmpty       Reason = "verifier_chain_empty"
	ReasonChainUnknown     Reason = "verifier_chain_family_unknown"
	ReasonSameFamily       Reason = "verifier_family_in_chain"
)

// Refusal is a classification or verifier check that failed closed. Reason is
// the stable code a caller records; the error text starts with it.
type Refusal struct {
	Reason Reason
	Detail string
}

func (r *Refusal) Error() string { return string(r.Reason) + ": " + r.Detail }

func refuse(reason Reason, format string, args ...any) *Refusal {
	return &Refusal{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// ReasonOf returns the recorded reason of a refusal, or "" for any other error.
func ReasonOf(err error) Reason {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Reason
	}
	return ""
}

// A rule matches a model name that starts with prefix. With exact set, the
// rule matches only the prefix itself or the prefix followed by '-' and more
// characters, so "o1" matches "o1" and "o1-mini" but not "o10" or "o1x".
// Without exact, the prefix ends in '-' and at least one character must
// follow it.
type rule struct {
	prefix string
	family Family
	exact  bool
}

var prefixRules = []rule{
	{"claude-", Anthropic, false},
	{"gpt-", OpenAI, false},
	{"chatgpt-", OpenAI, false},
	{"codex-", OpenAI, false},
	{"o1", OpenAI, true},
	{"o3", OpenAI, true},
	{"o4", OpenAI, true},
	{"gemini-", Google, false},
}

// vendorRules apply to a name of the form "<vendor>/<model>", and only where
// the vendor names the same family as the model. Any other path is unknown,
// so an operator family cannot contradict these either.
var vendorRules = map[string][]rule{
	"anthropic": {{"claude-", Anthropic, false}},
	"openai": {
		{"gpt-", OpenAI, false},
		{"chatgpt-", OpenAI, false},
		{"o0", OpenAI, true}, {"o1", OpenAI, true}, {"o2", OpenAI, true},
		{"o3", OpenAI, true}, {"o4", OpenAI, true}, {"o5", OpenAI, true},
		{"o6", OpenAI, true}, {"o7", OpenAI, true}, {"o8", OpenAI, true},
		{"o9", OpenAI, true},
	},
	"google": {{"gemini-", Google, false}},
	"models": {{"gemini-", Google, false}},
}

var aliases = map[string]Family{
	"opus":   Anthropic,
	"sonnet": Anthropic,
	"haiku":  Anthropic,
}

// knownFamilies is the closed set of labels an operator may give a route.
// "unknown" is not in it: an unknown family always refuses.
var knownFamilies = []Family{
	Anthropic, OpenAI, Google,
	"meta", "mistral", "deepseek", "qwen", "xai", "cohere", "microsoft", "amazon",
}

// KnownFamilies returns the labels an operator may give a route.
func KnownFamilies() []Family { return append([]Family(nil), knownFamilies...) }

// Builtin returns the family the built-in rules give model, or Unknown.
// Matching ignores case and surrounding space. A name with characters
// outside printable ASCII, or with more than one '/', is unknown.
func Builtin(model string) Family {
	m := strings.TrimSpace(model)
	for i := range len(m) {
		if m[i] <= ' ' || m[i] > '~' {
			return Unknown
		}
	}
	m = strings.ToLower(m)
	if f, ok := aliases[m]; ok {
		return f
	}
	rules := prefixRules
	if vendor, rest, ok := strings.Cut(m, "/"); ok {
		rules, m = vendorRules[vendor], rest
	}
	for _, r := range rules {
		if r.matches(m) {
			return r.family
		}
	}
	return Unknown
}

func (r rule) matches(m string) bool {
	rest, ok := strings.CutPrefix(m, r.prefix)
	if !ok || strings.Contains(rest, "/") {
		return false
	}
	if r.exact {
		return rest == "" || (len(rest) > 1 && rest[0] == '-')
	}
	return rest != ""
}

// NormalizeOverride returns an operator family label in its stored form
// (trimmed, lower case) and checks it is one of KnownFamilies. An empty
// label is valid and means no override.
func NormalizeOverride(override string) (string, error) {
	o := strings.ToLower(strings.TrimSpace(override))
	if o == "" {
		return "", nil
	}
	if !slices.Contains(knownFamilies, Family(o)) {
		return "", refuse(ReasonOverrideInvalid, "family %q is not one of %s", override, joinFamilies(knownFamilies))
	}
	return o, nil
}

func joinFamilies(fs []Family) string {
	names := make([]string, len(fs))
	for i, f := range fs {
		names[i] = string(f)
	}
	return strings.Join(names, ", ")
}

// Classify returns the family of model given the operator's family for its
// route (empty for none). It refuses an invalid override, an override that
// disagrees with a built-in rule, and a model whose family stays unknown.
func Classify(model, override string) (Family, error) {
	o, err := NormalizeOverride(override)
	if err != nil {
		return Unknown, err
	}
	if strings.TrimSpace(model) == "" {
		return Unknown, refuse(ReasonUnknown, "no model recorded")
	}
	if b := Builtin(model); b != Unknown {
		if o != "" && Family(o) != b {
			return Unknown, refuse(ReasonOverrideConflict, "model %q is %s by the built-in rules, not %s", model, b, o)
		}
		return b, nil
	}
	if o == "" {
		return Unknown, refuse(ReasonUnknown, "model %q matches no built-in family and its route names none", model)
	}
	return Family(o), nil
}
