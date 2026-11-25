package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
)

// LearningStore is an optional capability for dedicated versioned learning storage (ADR-0012).
type LearningStore interface {
	ProposeLearning(ctx context.Context, learning domain.Learning) (domain.Learning, error)
	GetLearning(ctx context.Context, id string) (domain.Learning, error)
	AcceptLearning(ctx context.Context, id, expectedDigest string, now time.Time) (domain.Learning, error)
	ConfirmLearning(ctx context.Context, id string, outcomeID int64, now time.Time) (domain.Learning, error)
	RetireLearning(ctx context.Context, id, reason string, now time.Time) (domain.Learning, error)
	ListLearnings(ctx context.Context, filter LearningFilter) ([]domain.Learning, error)
	RecordLearningInjection(ctx context.Context, learningID, packetID, taskID string, at time.Time) (bool, error)
	RecordLearningOutcome(ctx context.Context, outcome domain.LearningOutcome) (domain.LearningOutcome, error)
	ExpireLearnings(ctx context.Context, now time.Time) (int, error)
	ListLearningHistory(ctx context.Context, learningID string) ([]domain.LearningHistory, error)
	ListLearningOutcomes(ctx context.Context, learningID string) ([]domain.LearningOutcome, error)
}

var ErrLearningUnsupported = errors.New("store does not support dedicated learning records")

// Learnings acquires the optional LearningStore capability or fails closed.
func Learnings(st Store) (LearningStore, error) {
	if ls, ok := st.(LearningStore); ok {
		return ls, nil
	}
	return nil, ErrLearningUnsupported
}

const (
	MaxLearningStatementBytes      = 4 << 10
	MaxLearningFieldBytes          = 512
	MaxLearningOutcomeSummaryBytes = 2 << 10
	MaxLearningPathGlobs           = 16
	MaxLearningPathGlobBytes       = 256
	MaxLearningSelectorBytes       = 128
	MaxLearningSymptomBytes        = 256
	DefaultLearningListLimit       = 100
	MaxLearningListLimit           = 1000
	LearningTimestampLayout        = "2006-01-02T15:04:05.000000000Z"
)

// LearningFilter selects eligible advice by default. Specify proposed or retired
// status to review those records, or IncludeExpired to inspect recorded status.
type LearningFilter struct {
	Status               domain.LearningStatus
	ScopeKey             string
	GlobalOnly           bool
	RepoRemoteURL        string
	Paths                []string
	IncidentCluster      string
	IncidentNamespace    string
	IncidentWorkloadKind string
	IncidentSymptom      string
	NeedsScopeMapping    *bool
	IncludeExpired       bool
	Now                  time.Time
	Limit                int
}

type learningOperatorKey struct{}

// WithLearningOperator binds the identity established by operator authentication.
func WithLearningOperator(ctx context.Context, operator string) context.Context {
	return context.WithValue(ctx, learningOperatorKey{}, operator)
}

func LearningOperator(ctx context.Context) (string, bool) {
	operator, _ := ctx.Value(learningOperatorKey{}).(string)
	return operator, ValidLearningID(operator, MaxLearningFieldBytes)
}

// Match URL userinfo in prose, including percent-encoded credentials.
var learningURLCredentials = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/@?#[:space:]]+@`)

// RedactLearningText removes secrets and PEM private key blocks deterministically.
func RedactLearningText(text string) string {
	return RedactCouncilAnswer(learningURLCredentials.ReplaceAllString(text, "${1}[REDACTED:userinfo]@"))
}

// ContainsLearningSecret reports whether text contains a sensitive credential or private key header.
func ContainsLearningSecret(text string) bool {
	return learningURLCredentials.MatchString(text) || privateKeyHeader.MatchString(text) || redact.ContainsSensitive(text)
}

// FormatLearningTime formats a UTC timestamp with fixed-width nanoseconds so SQLite
// lexicographic comparisons match exact chronological ordering.
func FormatLearningTime(t time.Time) string {
	if t.IsZero() {
		t = time.Now().UTC()
	}
	return t.UTC().Format(LearningTimestampLayout)
}

// ParseLearningTime parses a stored RFC3339Nano timestamp in UTC.
func ParseLearningTime(raw string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// ValidLearningID reports whether s is a nonblank, bounded, control-free, secret-free identifier.
func ValidLearningID(s string, maxBytes int) bool {
	if s == "" || len(s) > maxBytes || strings.TrimSpace(s) != s || !utf8.ValidString(s) {
		return false
	}
	if strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) }) {
		return false
	}
	return !ContainsLearningSecret(s)
}

// ValidLearningText reports whether s is nonblank valid UTF-8 without NUL bytes within maxBytes.
func ValidLearningText(s string, maxBytes int) bool {
	if len(s) == 0 || len(s) > maxBytes || !utf8.ValidString(s) || strings.TrimSpace(s) == "" {
		return false
	}
	return !strings.ContainsRune(s, 0)
}

// ValidLearningStatus reports whether s is one of proposed, accepted, or retired.
func ValidLearningStatus(s domain.LearningStatus) bool {
	switch s {
	case domain.LearningStatusProposed, domain.LearningStatusAccepted, domain.LearningStatusRetired:
		return true
	default:
		return false
	}
}

// ValidLearningSourceKind reports whether s is a recognized learning source kind.
// Legacy is rejected when allowLegacy is false so only migration can emit legacy provenance.
func ValidLearningSourceKind(s domain.LearningSourceKind, allowLegacy bool) bool {
	switch s {
	case domain.LearningSourceOperator,
		domain.LearningSourceAgent,
		domain.LearningSourceVerifier,
		domain.LearningSourceShadewright,
		domain.LearningSourceTask,
		domain.LearningSourceCouncil,
		domain.LearningSourceIncident:
		return true
	case domain.LearningSourceLegacy:
		return allowLegacy
	default:
		return false
	}
}

// ValidLearningOutcomeKind reports whether k is supporting, contradiction, or inconclusive.
func ValidLearningOutcomeKind(k domain.LearningOutcomeKind) bool {
	switch k {
	case domain.LearningOutcomeSupporting, domain.LearningOutcomeContradiction, domain.LearningOutcomeInconclusive:
		return true
	default:
		return false
	}
}

// NormalizeRepoRemoteURL validates and canonicalizes a credential-free repository remote URL.
func NormalizeRepoRemoteURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if !ValidLearningID(s, MaxLearningFieldBytes) {
		return "", errors.New("invalid repository remote URL")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", errors.New("invalid repository remote URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" && scheme != "ssh" {
		return "", errors.New("repository remote URL requires https, http, or ssh scheme")
	}
	if u.User != nil {
		return "", errors.New("repository remote URL must not contain credentials")
	}
	if u.Host == "" || strings.ContainsAny(u.Host, " \t\r\n") {
		return "", errors.New("repository remote URL requires a host")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("repository remote URL must not contain query or fragment")
	}
	cleanedPath := strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git")
	if cleanedPath == "" || cleanedPath == "/" {
		return "", errors.New("repository remote URL requires a repository path")
	}
	for seg := range strings.SplitSeq(strings.TrimPrefix(cleanedPath, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", errors.New("repository remote URL path is invalid")
		}
	}
	escaped := strings.TrimSuffix(strings.TrimSuffix(u.EscapedPath(), "/"), ".git")
	canonical := scheme + "://" + strings.ToLower(u.Host) + escaped
	if len(canonical) > MaxLearningFieldBytes || ContainsLearningSecret(canonical) {
		return "", errors.New("invalid canonical repository remote URL")
	}
	return canonical, nil
}

// NormalizePathGlob validates a repository-relative path glob.
func NormalizePathGlob(raw string) (string, error) {
	g := strings.TrimSpace(raw)
	if g == "" || len(g) > MaxLearningPathGlobBytes || !utf8.ValidString(g) {
		return "", errors.New("invalid path glob")
	}
	if strings.ContainsFunc(g, func(r rune) bool { return unicode.IsControl(r) }) || ContainsLearningSecret(g) {
		return "", errors.New("invalid path glob")
	}
	if strings.HasPrefix(g, "/") || strings.HasPrefix(g, "~") || strings.Contains(g, "\\") {
		return "", errors.New("path glob must be repository-relative")
	}
	if len(g) >= 2 && g[1] == ':' && ((g[0] >= 'a' && g[0] <= 'z') || (g[0] >= 'A' && g[0] <= 'Z')) {
		return "", errors.New("path glob must be repository-relative")
	}
	g = strings.TrimPrefix(g, "./")
	if g == "" || g == "." || g == ".." {
		return "", errors.New("invalid path glob")
	}
	for seg := range strings.SplitSeq(g, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", errors.New("path glob contains invalid segment")
		}
	}
	syntaxCheck := strings.ReplaceAll(g, "**", "*")
	if _, err := path.Match(syntaxCheck, "sample"); err != nil {
		return "", errors.New("invalid path glob syntax")
	}
	return g, nil
}

// MatchesLearningPaths checks repository-relative paths. A ** segment matches
// zero or more whole path components. Other segments use Go path.Match.
func MatchesLearningPaths(globs, paths []string) bool {
	if len(globs) == 0 {
		return true
	}
	for _, candidate := range paths {
		candidate = strings.TrimPrefix(candidate, "./")
		if !fs.ValidPath(candidate) || candidate == "." || strings.ContainsAny(candidate, `\`) ||
			(len(candidate) >= 2 && candidate[1] == ':') {
			continue
		}
		parts := strings.Split(candidate, "/")
		for _, glob := range globs {
			matches := make([]bool, len(parts)+1)
			matches[0] = true
			for segment := range strings.SplitSeq(glob, "/") {
				if segment == "**" {
					for i := range parts {
						matches[i+1] = matches[i+1] || matches[i]
					}
				} else {
					for i := len(parts) - 1; i >= 0; i-- {
						ok, _ := path.Match(segment, parts[i])
						matches[i+1] = matches[i] && ok
					}
					matches[0] = false
				}
			}
			if matches[len(parts)] {
				return true
			}
		}
	}
	return false
}

// NormalizeLearningScope validates a scope and derives its scope kind, canonical scope key, and specificity.
func NormalizeLearningScope(in domain.LearningScope, allowLegacy bool) (domain.LearningScope, string, string, int, error) {
	if in.NeedsScopeMapping {
		if !allowLegacy {
			return domain.LearningScope{}, "", "", 0, errors.New("unmapped legacy scopes can only be created by migration")
		}
		ns := strings.TrimSpace(in.LegacyNamespace)
		if !ValidLearningText(ns, MaxLearningFieldBytes) {
			return domain.LearningScope{}, "", "", 0, errors.New("legacy scope requires legacy namespace")
		}
		ns = RedactLearningText(ns)
		out := domain.LearningScope{
			LegacyNamespace:   ns,
			NeedsScopeMapping: true,
		}
		return out, "legacy", ns, 0, nil
	}

	if in.Global {
		if strings.TrimSpace(in.RepoRemoteURL) != "" || len(in.PathGlobs) > 0 ||
			strings.TrimSpace(in.Cluster) != "" || strings.TrimSpace(in.Namespace) != "" ||
			strings.TrimSpace(in.WorkloadKind) != "" || strings.TrimSpace(in.Symptom) != "" {
			return domain.LearningScope{}, "", "", 0, errors.New("global scope cannot be combined with narrower selectors")
		}
		out := domain.LearningScope{Global: true}
		if allowLegacy && strings.TrimSpace(in.LegacyNamespace) != "" {
			out.LegacyNamespace = RedactLearningText(strings.TrimSpace(in.LegacyNamespace))
		}
		return out, "global", "global", 0, nil
	}

	var out domain.LearningScope
	if allowLegacy && strings.TrimSpace(in.LegacyNamespace) != "" {
		out.LegacyNamespace = RedactLearningText(strings.TrimSpace(in.LegacyNamespace))
	}

	if strings.TrimSpace(in.RepoRemoteURL) != "" {
		repo, err := NormalizeRepoRemoteURL(in.RepoRemoteURL)
		if err != nil {
			return domain.LearningScope{}, "", "", 0, err
		}
		out.RepoRemoteURL = repo
	}

	if len(in.PathGlobs) > MaxLearningPathGlobs {
		return domain.LearningScope{}, "", "", 0, errors.New("too many path globs")
	}
	if len(in.PathGlobs) > 0 {
		globs := make([]string, 0, len(in.PathGlobs))
		for _, raw := range in.PathGlobs {
			g, err := NormalizePathGlob(raw)
			if err != nil {
				return domain.LearningScope{}, "", "", 0, err
			}
			globs = append(globs, g)
		}
		slices.Sort(globs)
		out.PathGlobs = slices.Compact(globs)
	}

	normalizeSelector := func(raw string, maxBytes int) (string, error) {
		s := strings.TrimSpace(raw)
		if s == "" {
			return "", nil
		}
		if !ValidLearningID(s, maxBytes) {
			return "", errors.New("invalid incident selector")
		}
		return s, nil
	}
	var err error
	if out.Cluster, err = normalizeSelector(in.Cluster, MaxLearningSelectorBytes); err != nil {
		return domain.LearningScope{}, "", "", 0, err
	}
	if out.Namespace, err = normalizeSelector(in.Namespace, MaxLearningSelectorBytes); err != nil {
		return domain.LearningScope{}, "", "", 0, err
	}
	if out.WorkloadKind, err = normalizeSelector(in.WorkloadKind, MaxLearningSelectorBytes); err != nil {
		return domain.LearningScope{}, "", "", 0, err
	}
	if out.Symptom, err = normalizeSelector(in.Symptom, MaxLearningSymptomBytes); err != nil {
		return domain.LearningScope{}, "", "", 0, err
	}

	hasRepo := out.RepoRemoteURL != ""
	hasPath := len(out.PathGlobs) > 0
	hasIncident := out.Cluster != "" || out.Namespace != "" || out.WorkloadKind != "" || out.Symptom != ""
	if !hasRepo && !hasPath && !hasIncident {
		return domain.LearningScope{}, "", "", 0, errors.New("learning scope requires global or at least one selector")
	}

	kind := "compound"
	switch {
	case hasRepo && !hasPath && !hasIncident:
		kind = "repo"
	case !hasRepo && hasPath && !hasIncident:
		kind = "path"
	case !hasRepo && !hasPath && hasIncident:
		kind = "incident"
	}

	specificity := 0
	if hasRepo {
		specificity += 2
	}
	if hasPath {
		specificity += 3
	}
	if out.Cluster != "" {
		specificity += 2
	}
	if out.Namespace != "" {
		specificity += 2
	}
	if out.WorkloadKind != "" {
		specificity += 2
	}
	if out.Symptom != "" {
		specificity += 2
	}

	return out, kind, CanonicalLearningScopeKey(out), specificity, nil
}

// CanonicalLearningScopeKey builds a deterministic indexed key for a normalized scope.
func CanonicalLearningScopeKey(scope domain.LearningScope) string {
	if scope.NeedsScopeMapping {
		return scope.LegacyNamespace
	}
	if scope.Global {
		return "global"
	}
	encodePart := func(v string) string {
		if strings.ContainsAny(v, "|,") {
			return strconv.Quote(v)
		}
		return v
	}
	parts := make([]string, 0, 6)
	if scope.RepoRemoteURL != "" {
		parts = append(parts, "repo:"+encodePart(scope.RepoRemoteURL))
	}
	if len(scope.PathGlobs) > 0 {
		encoded := make([]string, len(scope.PathGlobs))
		for i, g := range scope.PathGlobs {
			encoded[i] = encodePart(g)
		}
		parts = append(parts, "path:"+strings.Join(encoded, ","))
	}
	if scope.Cluster != "" {
		parts = append(parts, "cluster:"+encodePart(scope.Cluster))
	}
	if scope.Namespace != "" {
		parts = append(parts, "namespace:"+encodePart(scope.Namespace))
	}
	if scope.WorkloadKind != "" {
		parts = append(parts, "kind:"+encodePart(scope.WorkloadKind))
	}
	if scope.Symptom != "" {
		parts = append(parts, "symptom:"+encodePart(scope.Symptom))
	}
	return strings.Join(parts, "|")
}

// ComputeLearningVersionDigest derives a deterministic SHA-256 digest over a learning's immutable fields.
func ComputeLearningVersionDigest(l domain.Learning) (string, error) {
	payload := struct {
		Statement       string                    `json:"statement"`
		Scope           domain.LearningScope      `json:"scope"`
		SourceKind      domain.LearningSourceKind `json:"sourceKind"`
		SourceID        string                    `json:"sourceID,omitempty"`
		EvidenceRef     string                    `json:"evidenceRef,omitempty"`
		PredecessorID   string                    `json:"predecessorID,omitempty"`
		LegacyNamespace string                    `json:"legacyNamespace,omitempty"`
		LegacyKey       string                    `json:"legacyKey,omitempty"`
	}{
		Statement:       l.Statement,
		Scope:           l.Scope,
		SourceKind:      l.SourceKind,
		SourceID:        l.SourceID,
		EvidenceRef:     l.EvidenceRef,
		PredecessorID:   l.PredecessorID,
		LegacyNamespace: l.LegacyNamespace,
		LegacyKey:       l.LegacyKey,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
