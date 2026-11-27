package contextpack

import (
	"context"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const (
	LearningNamespaceGlobal     = "learning:global"
	LearningNamespaceRepoPrefix = "learning:repo:"
	LearningNamespacePathPrefix = "learning:path:"

	// MaxPacketLearnings and MaxLearningSectionBytes bound the learnings section (ADR-0012).
	MaxPacketLearnings      = 10
	MaxLearningSectionBytes = 4 << 10

	// LocalModelProvider is the only Model Gate provider whose requests stay on this machine.
	LocalModelProvider = "local"
)

// NormalizeLearningNamespace converts user-facing scopes ("global", "repo:foo", "path:internal/**")
// into reserved Court Memory learning namespaces.
func NormalizeLearningNamespace(scope string) string {
	s := strings.TrimSpace(scope)
	if s == "" || s == "global" || s == LearningNamespaceGlobal {
		return LearningNamespaceGlobal
	}
	if strings.HasPrefix(s, "learning:") {
		return s
	}
	if strings.HasPrefix(s, "repo:") {
		return LearningNamespaceRepoPrefix + strings.TrimSpace(strings.TrimPrefix(s, "repo:"))
	}
	if strings.HasPrefix(s, "path:") {
		return LearningNamespacePathPrefix + strings.TrimSpace(strings.TrimPrefix(s, "path:"))
	}
	return "learning:" + s
}

// Destination holds the daemon facts that decide where model requests of a
// packet can go. The daemon fixes them once at startup. The zero value means
// the destination is unknown, so no packet carries learnings.
type Destination struct {
	// DockerAgents is true when long-lived agents run in the Docker adapter,
	// on its internal network without egress.
	DockerAgents bool
	// ModelGateway is true when a model gateway URL is set. The gateway then
	// receives requests for every provider, local included.
	ModelGateway bool
}

// LearningsAllowed reports whether a packet for wl may carry learnings.
// ADR-0012 keeps learnings out of remote and unknown destinations, so only a
// workload whose every model request stays on this machine qualifies.
func (d Destination) LearningsAllowed(wl domain.Workload) bool {
	if !d.DockerAgents || d.ModelGateway {
		return false
	}
	// DockerAgents only places long-lived agents, so every other class,
	// including an empty or unknown one, has an unknown destination.
	if wl.Spec.Class != domain.ClassLongLivedAgent || wl.Spec.Coding != nil {
		return false
	}
	grant := wl.Spec.Grants.ModelGate
	return grant.Allowed && len(grant.Providers) == 1 && grant.Providers[0] == LocalModelProvider
}

// MatchLearnings returns accepted, unexpired learnings from the dedicated
// learning store whose every scope selector matches. Legacy Court Memory
// learning rows are never read. A store without learning records, or any
// store error, yields none. An empty repoRemoteURL matches only learnings
// without a repository selector.
func MatchLearnings(ctx context.Context, st store.Store, repoRemoteURL string, changedPaths []string, limit int) []domain.Learning {
	if st == nil {
		return nil
	}
	ls, err := store.Learnings(st)
	if err != nil {
		return nil
	}
	if limit <= 0 {
		limit = MaxPacketLearnings
	}
	now := time.Now().UTC()
	matched, err := ls.ListLearnings(ctx, store.LearningFilter{
		Status:        domain.LearningStatusAccepted,
		RepoRemoteURL: repoRemoteURL,
		Paths:         changedPaths,
		Now:           now,
		Limit:         limit,
	})
	if err != nil {
		return nil
	}
	eligible := matched[:0]
	for _, l := range matched {
		if l.IsEligible(now) {
			eligible = append(eligible, l)
		}
	}
	return eligible
}

// learningSection renders up to MaxPacketLearnings redacted entries, most
// specific first, within MaxLearningSectionBytes. Overflow is omitted.
func learningSection(learnings []domain.Learning) []packetContext {
	var lines []packetContext
	total := 0
	for _, l := range learnings {
		if len(lines) == MaxPacketLearnings {
			break
		}
		// RedactLearningText adds URL userinfo and unclosed private key
		// blocks to the shared rules, so a row that skipped write-time
		// redaction is still cleaned here. The newline stays outside, because
		// an unclosed key block consumes the rest of the text.
		line := store.RedactLearningText("- ["+l.ID+"] "+strings.TrimSpace(l.Statement)) + "\n"
		if total+len(line) > MaxLearningSectionBytes {
			break
		}
		total += len(line)
		lines = append(lines, packetContext{section: "Scoped Learnings", text: line, at: l.UpdatedAt})
	}
	return lines
}
