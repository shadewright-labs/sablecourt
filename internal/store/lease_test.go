package store

import (
	"database/sql"
	"slices"
	"testing"
)

// TestPinnedProvidersRoundTrip checks that only NULL reads as unpinned and
// that a corrupt value fails instead of widening the lease.
func TestPinnedProvidersRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      sql.NullString
		want    []string
		wantErr bool
	}{
		{"null is unpinned", sql.NullString{}, nil, false},
		{"local only", sql.NullString{String: `["local"]`, Valid: true}, []string{"local"}, false},
		{"empty pin reaches none", sql.NullString{String: `[]`, Valid: true}, []string{}, false},
		{"json null is refused", sql.NullString{String: `null`, Valid: true}, nil, true},
		{"empty string is refused", sql.NullString{String: ``, Valid: true}, nil, true},
		{"object is refused", sql.NullString{String: `{"local":true}`, Valid: true}, nil, true},
		{"numbers are refused", sql.NullString{String: `[1]`, Valid: true}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodePinnedProviders(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if (got == nil) != (tc.want == nil) || !slices.Equal(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
			enc, err := EncodePinnedProviders(got)
			if err != nil {
				t.Fatal(err)
			}
			if enc != tc.in {
				t.Fatalf("encode = %#v, want %#v", enc, tc.in)
			}
		})
	}
}
