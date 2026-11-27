// Package modelfamily is the daemon's one classifier of model families and
// the check that a workflow verifier step runs a family no model in its
// chain ran (ADR-0010, amendment of 2026-11-27).
//
// Built-in prefix rules always apply to the names they recognize. An
// operator may give a family only to a model no rule recognizes, and a family
// that disagrees with a rule is refused. Any other name is unknown, and an
// unknown family refuses.
package modelfamily

import (
	"errors"
	"fmt"
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

// maxFamilyLen bounds an operator family label.
const maxFamilyLen = 32

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

var prefixRules = []struct {
	prefix string
	family Family
}{
	{"claude-", Anthropic},
	{"gpt-", OpenAI},
	{"codex-", OpenAI},
	{"o1", OpenAI},
	{"o3", OpenAI},
	{"o4", OpenAI},
	{"gemini-", Google},
}

var aliases = map[string]Family{
	"opus":   Anthropic,
	"sonnet": Anthropic,
	"haiku":  Anthropic,
}

// Builtin returns the family the built-in prefix rules give model, or Unknown.
// Matching ignores case and surrounding space.
func Builtin(model string) Family {
	m := strings.ToLower(strings.TrimSpace(model))
	if f, ok := aliases[m]; ok {
		return f
	}
	for _, r := range prefixRules {
		if strings.HasPrefix(m, r.prefix) {
			return r.family
		}
	}
	return Unknown
}

// NormalizeOverride returns an operator family label in its stored form
// (trimmed, lower case) and checks its syntax. An empty label is valid and
// means no override.
func NormalizeOverride(override string) (string, error) {
	o := strings.ToLower(strings.TrimSpace(override))
	if o == "" {
		return "", nil
	}
	if len(o) > maxFamilyLen || o[0] < 'a' || o[0] > 'z' {
		return "", refuse(ReasonOverrideInvalid, "family %q must start with a letter and be at most %d characters", override, maxFamilyLen)
	}
	for _, c := range o {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return "", refuse(ReasonOverrideInvalid, "family %q may hold only letters, digits and '-'", override)
		}
	}
	return o, nil
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
