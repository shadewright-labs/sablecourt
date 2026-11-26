package store_test

import (
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// githubToken builds a token-shaped value at run time so no key-shaped literal is in the source.
func githubToken() string { return "ghp_" + strings.Repeat("a1B2", 9) }

func TestNormalizeRepoRemoteURL(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"https with git suffix", "https://github.com/org/repo.git", "https://github.com/org/repo"},
		{"host is lowercased, path case kept", "HTTPS://GitHub.COM/Org/Repo/", "https://github.com/Org/Repo"},
		{"surrounding space", "  https://example.test/org/repo  ", "https://example.test/org/repo"},
		{"http", "http://example.test/a/b", "http://example.test/a/b"},
		{"ssh without user", "ssh://example.test/org/repo.git", "ssh://example.test/org/repo"},
		{"host port kept", "https://example.test:8443/org/repo", "https://example.test:8443/org/repo"},
		{"repeated git suffix kept", "https://example.test/org/repo.git.git", "https://example.test/org/repo.git.git"},
		{"escaped slash kept escaped", "https://example.test/org/a%2Fb", "https://example.test/org/a%2Fb"},

		{"empty", "", ""},
		{"blank", "   ", ""},
		{"user and password", "https://user:" + "pw@example.test/org/repo", ""},
		{"token as user", "https://" + githubToken() + "@example.test/org/repo", ""},
		{"percent encoded user", "https://us%40er@example.test/org/repo", ""},
		{"empty userinfo", "https://@example.test/org/repo", ""},
		{"ssh with git user", "ssh://git@example.test/org/repo", ""},
		{"scp form", "git@example.test:org/repo.git", ""},
		{"file scheme", "file:///etc/passwd", ""},
		{"ftp scheme", "ftp://example.test/org/repo", ""},
		{"no scheme", "example.test/org/repo", ""},
		{"javascript scheme", "javascript:alert(1)", ""},
		{"empty host", "https:///org/repo", ""},
		{"query", "https://example.test/org/repo?ref=main", ""},
		{"fragment", "https://example.test/org/repo#readme", ""},
		{"no path", "https://example.test", ""},
		{"root path", "https://example.test/", ""},
		{"only git suffix", "https://example.test/.git", ""},
		{"dot dot segment", "https://example.test/org/../repo", ""},
		{"dot segment", "https://example.test/org/./repo", ""},
		{"empty segment", "https://example.test/org//repo", ""},
		{"NUL byte", "https://example.test/org/re\x00po", ""},
		{"newline", "https://example.test/org/re\npo", ""},
		{"DEL byte", "https://example.test/org/repo\x7f", ""},
		{"invalid UTF-8", "https://example.test/org/\xff", ""},
		{"oversize", "https://example.test/" + strings.Repeat("a", store.MaxLearningFieldBytes), ""},
		{"token in path", "https://example.test/org/" + githubToken(), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.NormalizeRepoRemoteURL(tc.raw)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("NormalizeRepoRemoteURL(%q) = %q, want an error", tc.raw, got)
				}
				if got != "" {
					t.Fatalf("NormalizeRepoRemoteURL(%q) returned %q with an error", tc.raw, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("NormalizeRepoRemoteURL(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
			}
		})
	}
}

func TestNormalizePathGlobCorpus(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"plain", "src/*.go", "src/*.go"},
		{"double star", "**/*.go", "**/*.go"},
		{"dot slash prefix", "./internal/store/**", "internal/store/**"},
		{"trimmed", "  docs/*.md  ", "docs/*.md"},
		{"character class", "cmd/[a-z]*/main.go", "cmd/[a-z]*/main.go"},

		{"empty", "", ""},
		{"blank", "   ", ""},
		{"only dot slash", "./", ""},
		{"dot", ".", ""},
		{"dot dot", "..", ""},
		{"absolute", "/etc/passwd", ""},
		{"home", "~/notes", ""},
		{"backslash", `src\*.go`, ""},
		{"windows drive", "C:/src/*.go", ""},
		{"drive without slash", "d:src", ""},
		{"traversal", "src/../../etc/*", ""},
		{"leading traversal", "../secret", ""},
		{"dot segment", "src/./a.go", ""},
		{"empty segment", "src//a.go", ""},
		{"trailing slash", "src/", ""},
		{"bad class", "src/[a-", ""},
		{"tab", "src/\ta.go", ""},
		{"NUL", "src/a\x00.go", ""},
		{"escape sequence", "src/\x1b[31m", ""},
		{"invalid UTF-8", "src/\xfe.go", ""},
		{"oversize", strings.Repeat("a", store.MaxLearningPathGlobBytes+1), ""},
		{"secret", "src/" + githubToken(), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.NormalizePathGlob(tc.raw)
			if tc.want == "" {
				if err == nil || got != "" {
					t.Fatalf("NormalizePathGlob(%q) = %q, %v; want a refusal", tc.raw, got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("NormalizePathGlob(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
			}
		})
	}
	if got, err := store.NormalizePathGlob(strings.Repeat("a", store.MaxLearningPathGlobBytes)); err != nil || len(got) != store.MaxLearningPathGlobBytes {
		t.Fatalf("glob at the size limit = %d bytes, %v", len(got), err)
	}
}

func TestMatchesLearningPaths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		globs []string
		paths []string
		want  bool
	}{
		{"no globs matches everything", nil, nil, true},
		{"no paths", []string{"src/*.go"}, nil, false},
		{"single segment", []string{"src/*.go"}, []string{"src/a.go"}, true},
		{"star does not cross segments", []string{"src/*.go"}, []string{"src/x/a.go"}, false},
		{"double star zero segments", []string{"**/*.go"}, []string{"a.go"}, true},
		{"double star many segments", []string{"**/*.go"}, []string{"a/b/c/d.go"}, true},
		{"trailing double star matches dir", []string{"src/**"}, []string{"src"}, true},
		{"trailing double star matches deep", []string{"src/**"}, []string{"src/a/b/c"}, true},
		{"middle double star", []string{"a/**/z.go"}, []string{"a/z.go", "q"}, true},
		{"middle double star wrong tail", []string{"a/**/z.go"}, []string{"a/b/y.go"}, false},
		{"glob longer than path", []string{"a/b/c"}, []string{"a/b"}, false},
		{"second glob matches", []string{"docs/*", "src/*.go"}, []string{"src/a.go"}, true},
		{"dot slash candidate", []string{"src/*.go"}, []string{"./src/a.go"}, true},
		{"traversal candidate skipped", []string{"**"}, []string{"../etc/passwd"}, false},
		{"absolute candidate skipped", []string{"**"}, []string{"/etc/passwd"}, false},
		{"backslash candidate skipped", []string{"**"}, []string{`src\a.go`}, false},
		{"drive candidate skipped", []string{"**"}, []string{"C:/src/a.go"}, false},
		{"dot candidate skipped", []string{"**"}, []string{"."}, false},
		{"empty candidate skipped", []string{"**"}, []string{""}, false},
		{"bad candidate does not hide a good one", []string{"src/*.go"}, []string{"../src/a.go", "src/b.go"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := store.MatchesLearningPaths(tc.globs, tc.paths); got != tc.want {
				t.Fatalf("MatchesLearningPaths(%q, %q) = %v, want %v", tc.globs, tc.paths, got, tc.want)
			}
		})
	}
}

func TestNormalizeLearningScope(t *testing.T) {
	tooMany := make([]string, store.MaxLearningPathGlobs+1)
	for i := range tooMany {
		tooMany[i] = "src/*.go"
	}
	for _, tc := range []struct {
		name        string
		in          domain.LearningScope
		allowLegacy bool
		wantErr     bool
		want        domain.LearningScope
		kind, key   string
		specificity int
	}{
		{name: "global", in: domain.LearningScope{Global: true}, want: domain.LearningScope{Global: true}, kind: "global", key: "global"},
		{name: "global drops legacy namespace without migration", in: domain.LearningScope{Global: true, LegacyNamespace: "learning:old"},
			want: domain.LearningScope{Global: true}, kind: "global", key: "global"},
		{name: "global keeps legacy namespace in migration", in: domain.LearningScope{Global: true, LegacyNamespace: " learning:old "}, allowLegacy: true,
			want: domain.LearningScope{Global: true, LegacyNamespace: "learning:old"}, kind: "global", key: "global"},
		{name: "global with repo", in: domain.LearningScope{Global: true, RepoRemoteURL: "https://example.test/o/r"}, wantErr: true},
		{name: "global with path", in: domain.LearningScope{Global: true, PathGlobs: []string{"a"}}, wantErr: true},
		{name: "global with cluster", in: domain.LearningScope{Global: true, Cluster: "c"}, wantErr: true},
		{name: "global with namespace", in: domain.LearningScope{Global: true, Namespace: "n"}, wantErr: true},
		{name: "global with kind", in: domain.LearningScope{Global: true, WorkloadKind: "Deployment"}, wantErr: true},
		{name: "global with symptom", in: domain.LearningScope{Global: true, Symptom: "OOMKilled"}, wantErr: true},

		{name: "legacy needs migration", in: domain.LearningScope{NeedsScopeMapping: true, LegacyNamespace: "learning:x"}, wantErr: true},
		{name: "legacy needs namespace", in: domain.LearningScope{NeedsScopeMapping: true, LegacyNamespace: "  "}, allowLegacy: true, wantErr: true},
		{name: "legacy namespace with NUL", in: domain.LearningScope{NeedsScopeMapping: true, LegacyNamespace: "a\x00b"}, allowLegacy: true, wantErr: true},
		{name: "legacy mapping", in: domain.LearningScope{NeedsScopeMapping: true, LegacyNamespace: " learning:x ", Cluster: "ignored"}, allowLegacy: true,
			want: domain.LearningScope{NeedsScopeMapping: true, LegacyNamespace: "learning:x"}, kind: "legacy", key: "learning:x"},
		{name: "legacy namespace secret is redacted", in: domain.LearningScope{NeedsScopeMapping: true, LegacyNamespace: "https://" + githubToken() + "@example.test/x"}, allowLegacy: true,
			want: domain.LearningScope{NeedsScopeMapping: true, LegacyNamespace: "https://[REDACTED:userinfo]@example.test/x"}, kind: "legacy", key: "https://[REDACTED:userinfo]@example.test/x"},

		{name: "empty scope", in: domain.LearningScope{}, wantErr: true},
		{name: "blank selectors", in: domain.LearningScope{Cluster: "  ", Namespace: "\t"}, wantErr: true},
		{name: "repo", in: domain.LearningScope{RepoRemoteURL: "https://Example.test/o/r.git"},
			want: domain.LearningScope{RepoRemoteURL: "https://example.test/o/r"}, kind: "repo", key: "repo:https://example.test/o/r", specificity: 2},
		{name: "repo with credentials", in: domain.LearningScope{RepoRemoteURL: "https://u:" + "p@example.test/o/r"}, wantErr: true},
		{name: "paths are sorted and deduplicated", in: domain.LearningScope{PathGlobs: []string{"./src/*.go", "docs/*", "src/*.go"}},
			want: domain.LearningScope{PathGlobs: []string{"docs/*", "src/*.go"}}, kind: "path", key: `path:"docs/*","src/*.go"`, specificity: 3},
		{name: "path traversal", in: domain.LearningScope{PathGlobs: []string{"src/*.go", "../x"}}, wantErr: true},
		{name: "too many paths", in: domain.LearningScope{PathGlobs: tooMany}, wantErr: true},
		{name: "incident", in: domain.LearningScope{Cluster: " prod ", Namespace: "web", WorkloadKind: "Deployment", Symptom: "CrashLoopBackOff"},
			want: domain.LearningScope{Cluster: "prod", Namespace: "web", WorkloadKind: "Deployment", Symptom: "CrashLoopBackOff"},
			kind: "incident", key: "cluster:prod|namespace:web|kind:Deployment|symptom:CrashLoopBackOff", specificity: 8},
		{name: "compound", in: domain.LearningScope{RepoRemoteURL: "https://example.test/o/r", PathGlobs: []string{"a/*"}, Namespace: "web"},
			want: domain.LearningScope{RepoRemoteURL: "https://example.test/o/r", PathGlobs: []string{"a/*"}, Namespace: "web"},
			kind: "compound", key: `repo:https://example.test/o/r|path:"a/*"|namespace:web`, specificity: 7},
		{name: "compound legacy namespace kept in migration", in: domain.LearningScope{Cluster: "c", LegacyNamespace: "learning:c"}, allowLegacy: true,
			want: domain.LearningScope{Cluster: "c", LegacyNamespace: "learning:c"}, kind: "incident", key: "cluster:c", specificity: 2},
		{name: "cluster with control char", in: domain.LearningScope{Cluster: "pro\x07d"}, wantErr: true},
		{name: "namespace with secret", in: domain.LearningScope{Namespace: githubToken()}, wantErr: true},
		{name: "kind oversize", in: domain.LearningScope{WorkloadKind: strings.Repeat("k", store.MaxLearningSelectorBytes+1)}, wantErr: true},
		{name: "symptom oversize", in: domain.LearningScope{Symptom: strings.Repeat("s", store.MaxLearningSymptomBytes+1)}, wantErr: true},
		{name: "symptom at its own limit", in: domain.LearningScope{Symptom: strings.Repeat("s", store.MaxLearningSymptomBytes)},
			want: domain.LearningScope{Symptom: strings.Repeat("s", store.MaxLearningSymptomBytes)}, kind: "incident",
			key: "symptom:" + strings.Repeat("s", store.MaxLearningSymptomBytes), specificity: 2},
		{name: "separator in selector is quoted", in: domain.LearningScope{Cluster: "a|namespace:b"},
			want: domain.LearningScope{Cluster: "a|namespace:b"}, kind: "incident", key: `cluster:"a|namespace:b"`, specificity: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, kind, key, specificity, err := store.NormalizeLearningScope(tc.in, tc.allowLegacy)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NormalizeLearningScope(%+v) = %+v, %q, %q; want an error", tc.in, got, kind, key)
				}
				if kind != "" || key != "" || specificity != 0 {
					t.Fatalf("refused scope leaked kind %q key %q specificity %d", kind, key, specificity)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !scopeEqual(got, tc.want) || kind != tc.kind || key != tc.key || specificity != tc.specificity {
				t.Fatalf("NormalizeLearningScope(%+v) = %+v, %q, %q, %d; want %+v, %q, %q, %d",
					tc.in, got, kind, key, specificity, tc.want, tc.kind, tc.key, tc.specificity)
			}
			if again := store.CanonicalLearningScopeKey(got); again != key {
				t.Fatalf("CanonicalLearningScopeKey(normalized) = %q, want %q", again, key)
			}
		})
	}
}

func scopeEqual(a, b domain.LearningScope) bool {
	return a.Global == b.Global && a.RepoRemoteURL == b.RepoRemoteURL && strings.Join(a.PathGlobs, "\x00") == strings.Join(b.PathGlobs, "\x00") &&
		len(a.PathGlobs) == len(b.PathGlobs) && a.Cluster == b.Cluster && a.Namespace == b.Namespace && a.WorkloadKind == b.WorkloadKind &&
		a.Symptom == b.Symptom && a.LegacyNamespace == b.LegacyNamespace && a.NeedsScopeMapping == b.NeedsScopeMapping
}

// Selectors that embed the key separators must not collide with a scope that really has those selectors.
func TestCanonicalLearningScopeKeyInjection(t *testing.T) {
	pairs := [][2]domain.LearningScope{
		{{Cluster: "a|namespace:b"}, {Cluster: "a", Namespace: "b"}},
		{{Namespace: "n|kind:Pod"}, {Namespace: "n", WorkloadKind: "Pod"}},
		{{WorkloadKind: "k|symptom:x"}, {WorkloadKind: "k", Symptom: "x"}},
		{{RepoRemoteURL: "https://example.test/o/r|cluster:c"}, {RepoRemoteURL: "https://example.test/o/r", Cluster: "c"}},
		{{PathGlobs: []string{`a","b`}}, {PathGlobs: []string{"a", "b"}}},
		{{Symptom: "s,t"}, {Symptom: "s", Cluster: "t"}},
	}
	for _, p := range pairs {
		a, b := store.CanonicalLearningScopeKey(p[0]), store.CanonicalLearningScopeKey(p[1])
		if a == b {
			t.Errorf("scopes %+v and %+v share key %q", p[0], p[1], a)
		}
	}
	for _, tc := range []struct {
		scope domain.LearningScope
		want  string
	}{
		{domain.LearningScope{NeedsScopeMapping: true, LegacyNamespace: "learning:x", Global: true}, "learning:x"},
		{domain.LearningScope{Global: true, Cluster: "ignored"}, "global"},
		{domain.LearningScope{Symptom: "a,b"}, `symptom:"a,b"`},
		{domain.LearningScope{RepoRemoteURL: "r", PathGlobs: []string{"p"}, Cluster: "c", Namespace: "n", WorkloadKind: "k", Symptom: "s"},
			`repo:r|path:"p"|cluster:c|namespace:n|kind:k|symptom:s`},
		{domain.LearningScope{}, ""},
	} {
		if got := store.CanonicalLearningScopeKey(tc.scope); got != tc.want {
			t.Errorf("CanonicalLearningScopeKey(%+v) = %q, want %q", tc.scope, got, tc.want)
		}
	}
}

func TestValidLearningPredicates(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    string
		max  int
		id   bool
		text bool
	}{
		{"plain", "operator-1", 64, true, true},
		{"empty", "", 64, false, false},
		{"blank", "   ", 64, false, false},
		{"padded", " id ", 64, false, true},
		{"oversize", "abcdef", 5, false, false},
		{"at size", "abcde", 5, true, true},
		{"NUL", "a\x00b", 64, false, false},
		{"control", "a\x1bb", 64, false, true},
		{"newline inside", "line one\nline two", 64, false, true},
		{"invalid UTF-8", "a\xffb", 64, false, false},
		{"secret", githubToken(), 64, false, true},
		{"url credentials", "https://u:" + "p@example.test", 64, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := store.ValidLearningID(tc.s, tc.max); got != tc.id {
				t.Errorf("ValidLearningID(%q, %d) = %v, want %v", tc.s, tc.max, got, tc.id)
			}
			if got := store.ValidLearningText(tc.s, tc.max); got != tc.text {
				t.Errorf("ValidLearningText(%q, %d) = %v, want %v", tc.s, tc.max, got, tc.text)
			}
		})
	}

	for s, want := range map[domain.LearningStatus]bool{
		domain.LearningStatusProposed: true, domain.LearningStatusAccepted: true, domain.LearningStatusRetired: true,
		"": false, "Accepted": false, "deleted": false,
	} {
		if got := store.ValidLearningStatus(s); got != want {
			t.Errorf("ValidLearningStatus(%q) = %v, want %v", s, got, want)
		}
	}
	for k, want := range map[domain.LearningOutcomeKind]bool{
		domain.LearningOutcomeSupporting: true, domain.LearningOutcomeContradiction: true, domain.LearningOutcomeInconclusive: true,
		"": false, "supports": false,
	} {
		if got := store.ValidLearningOutcomeKind(k); got != want {
			t.Errorf("ValidLearningOutcomeKind(%q) = %v, want %v", k, got, want)
		}
	}
	for _, kind := range []domain.LearningSourceKind{domain.LearningSourceOperator, domain.LearningSourceAgent, domain.LearningSourceVerifier,
		domain.LearningSourceShadewright, domain.LearningSourceTask, domain.LearningSourceCouncil, domain.LearningSourceIncident} {
		if !store.ValidLearningSourceKind(kind, false) {
			t.Errorf("ValidLearningSourceKind(%q, false) = false", kind)
		}
	}
	if store.ValidLearningSourceKind(domain.LearningSourceLegacy, false) || !store.ValidLearningSourceKind(domain.LearningSourceLegacy, true) {
		t.Error("legacy source kind must be accepted only in migration")
	}
	if store.ValidLearningSourceKind("human", true) {
		t.Error("unknown source kind accepted")
	}
}
