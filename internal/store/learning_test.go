package store

import "testing"

func TestNormalizePathGlobPrefix(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"./C:/src/*.go", ""},
		{"./c:/src/*.go", ""},
		{"./~/src/*.go", ""},
		{"./src/*.go", "src/*.go"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := NormalizePathGlob(tc.input)
			if got != tc.want || (err == nil) != (tc.want != "") {
				t.Fatalf("NormalizePathGlob(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
			}
		})
	}
}
