package mcpgate

import (
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/approval"
)

// Low 7: the canonical encoding of each typed action input is fixed. The
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
	} {
		canonical, digest, err := approval.CanonicalizeAndHash(tc.value)
		if err != nil {
			t.Fatal(err)
		}
		if string(canonical) != tc.canonical || digest != tc.digest {
			t.Errorf("%s: canonical %s digest %s, want %s %s", tc.name, canonical, digest, tc.canonical, tc.digest)
		}
	}
}
