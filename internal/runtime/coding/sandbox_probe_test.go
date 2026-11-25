package coding

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfinementProbe(t *testing.T) {
	for _, tc := range []struct {
		name, read, write string
		wantError         bool
	}{
		{"confined", "deny", "deny", false},
		{"read allowed", "allow", "deny", true},
		{"write allowed", "deny", "allow", true},
		{"unconfined", "allow", "allow", true},
		{"reader failed", "broken", "deny", true},
		{"writer failed", "deny", "broken", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			scratch := t.TempDir()
			t.Setenv("TMPDIR", scratch)
			bin := filepath.Join(dir, "probe")
			script := "#!/bin/sh\n" + `
case "${3##*/}" in
true) exit 0 ;;
cat) mode=` + tc.read + ` ;;
tee) mode=` + tc.write + ` ;;
*) exit 2 ;;
esac
case "$mode" in
allow) exec "$3" "$4" ;;
deny) printf '%s: %s: Operation not permitted\n' "${3##*/}" "$4" >&2; exit 1 ;;
*) exit 2 ;;
esac
`
			if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			err := probeSeatbelt(t.Context(), bin)
			if (err != nil) != tc.wantError {
				t.Errorf("error = %v, want error %v", err, tc.wantError)
			}
			items, err := os.ReadDir(scratch)
			if err != nil || len(items) != 0 {
				t.Errorf("probe left files: %v, error %v", items, err)
			}
		})
	}
}
