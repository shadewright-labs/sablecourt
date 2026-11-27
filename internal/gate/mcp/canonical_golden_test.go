package mcpgate

import (
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/approval"
)

// The canonical encoding of each typed action input is fixed. The
// approval digest is the SHA-256 of json.Marshal of the typed value, so a
// change to a struct (a field added, renamed, reordered, or its omitempty
// changed) changes every digest. Approvals pending across such an upgrade then
// fail at consumption with approval_mismatch_or_unapproved and need a new
// approval. If this test fails, that change was made: decide whether it is
// worth it, then update the golden values and say so in the release notes.
func TestCanonicalEncodingOfTypedActionsIsFixed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		value     any
		canonical string
		digest    string
	}{
		{
			"git.open_pr",
			OpenPRInput{Owner: "acme", Repo: "widgets", Head: "sablecourt/fix-1", Base: "main", Title: "Fix", Body: "Details"},
			`{"owner":"acme","repo":"widgets","head":"sablecourt/fix-1","base":"main","title":"Fix","body":"Details"}`,
			"1351eb3ee3ab38b2b40e8189f3047cc3afee012dc458f484158343a8992d5b76",
		},
		{
			"git.create_branch",
			CreateBranchInput{Branch: "sablecourt/fix-1", Base: "main"},
			`{"branch":"sablecourt/fix-1","base":"main"}`,
			"2d09d722b75e1ed4fa59be98585f7478aa2901739c40ff24738bdd2626ed39a6",
		},
		{
			"sandbox.propose_promotion",
			PromoteSandboxInput{Workload: "exp-sandbox", Branch: "sablecourt/promote", Reason: "ready"},
			`{"workload":"exp-sandbox","branch":"sablecourt/promote","reason":"ready"}`,
			"8106b8cf55ff86eada948b977dcb81e8d5d52e5e5f4095aa34e57c0134cccfa6",
		},
		{
			"git.push",
			PushBranchInput{Branch: "sablecourt/task-1", CommitSHA: strings.Repeat("ab", 20), Destination: "https://github.com/acme/widgets.git"},
			`{"branch":"sablecourt/task-1","commit_sha":"abababababababababababababababababababab","destination":"https://github.com/acme/widgets.git"}`,
			"c08b0e33f4cab658da5a5e2f5587613734d8777ff6d6fbfb3c390667b85ee8e2",
		},
		{
			"git.merge_pr",
			MergePRInput{Owner: "acme", Repo: "widgets", PRNumber: 7, CommitSHA: strings.Repeat("ab", 20), MergeMethod: "squash"},
			`{"owner":"acme","repo":"widgets","pr_number":7,"commit_sha":"abababababababababababababababababababab","merge_method":"squash"}`,
			"a37c93a677d6343314460f3410559a66268b4d0fa895fbf703051343be81c0ec",
		},
		{
			"git.pr_checks",
			PRChecksInput{Owner: "acme", Repo: "widgets", PRNumber: 7},
			`{"owner":"acme","repo":"widgets","pr_number":7}`,
			"f99e4881186adaf08ecbd3d6ea654155d274d45c155c2e076c381459d4a5504b",
		},
	} {
		canonical, digest, err := approval.CanonicalizeAndHash(tc.value)
		if err != nil {
			t.Fatal(err)
		}
		if canonical != tc.canonical || digest != tc.digest {
			t.Errorf("%s: canonical %s digest %s, want %s %s", tc.name, canonical, digest, tc.canonical, tc.digest)
		}
	}
}
