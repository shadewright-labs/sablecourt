package coding

import (
	"strings"
	"testing"
)

// TestRedactDiffTextHidesEveryUnfinishedKey is the case of issue 243: two
// private keys with no END line in one diff. Slicing from the last BEGIN
// line kept the first key's body.
func TestRedactDiffTextHidesEveryUnfinishedKey(t *testing.T) {
	t.Parallel()
	begin := "-----BEGIN " + "PRIVATE KEY-----"
	end := "-----END " + "PRIVATE KEY-----"
	for name, tc := range map[string]struct {
		diff, keep string
		hidden     []string
	}{
		"two unfinished keys": {
			diff:   "+" + begin + "\n+SECRET_KEY_1_BODY\n+" + begin + "\n+SECRET_KEY_2_BODY\n",
			keep:   "+[REDACTED:private_key]",
			hidden: []string{"SECRET_KEY_1_BODY", "SECRET_KEY_2_BODY"},
		},
		"whole key then unfinished key": {
			diff:   " context\n+" + begin + "\n+SECRET_KEY_1_BODY\n+" + end + "\n+kept line\n+" + begin + "\n+SECRET_KEY_2_BODY\n",
			keep:   " context\n+[REDACTED:private_key]\n+kept line\n+[REDACTED:private_key]",
			hidden: []string{"SECRET_KEY_1_BODY", "SECRET_KEY_2_BODY"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := redactDiffText(tc.diff)
			for _, h := range tc.hidden {
				if strings.Contains(got, h) {
					t.Fatalf("redactDiffText kept %q: %q", h, got)
				}
			}
			if got != tc.keep {
				t.Fatalf("redactDiffText = %q, want %q", got, tc.keep)
			}
		})
	}
}
