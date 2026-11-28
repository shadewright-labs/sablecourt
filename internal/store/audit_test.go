package store

import (
	"errors"
	"fmt"
	"testing"
)

func TestAuditFailureText(t *testing.T) {
	refused := fmt.Errorf("%w (task.submit submitted)", ErrAuditWrite)
	cases := []struct {
		name   string
		err    error
		want   string
		wantOK bool
	}{
		{"nil", nil, "", false},
		{"other error", errors.New("conflict"), "", false},
		{"refused", refused, "task submission refused: its audit record could not be written, nothing was changed", true},
		{"refused and wrapped", fmt.Errorf("wake vassal: %w", refused), "task submission refused: its audit record could not be written, nothing was changed", true},
		{"after effect", AfterEffect("task t1 was created", refused), "task t1 was created, but an audit record could not be written", true},
		{"after effect wrapped and joined", errors.Join(errors.New("cleanup"), fmt.Errorf("x: %w", AfterEffect("message 3 was stored", refused))), "message 3 was stored, but an audit record could not be written", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := AuditFailureText(tc.err, "task submission")
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("got %q %v, want %q %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
	if AfterEffect("x", nil) != nil {
		t.Fatal("AfterEffect(nil) is not nil")
	}
}
