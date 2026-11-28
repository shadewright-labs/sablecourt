package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// EncodePinnedProviders stores a lease's pinned Model Gate providers. A nil
// list is NULL (the packet carried no learnings); any other list, empty
// included, is a JSON array.
func EncodePinnedProviders(pinned []string) (sql.NullString, error) {
	if pinned == nil {
		return sql.NullString{}, nil
	}
	raw, err := json.Marshal(pinned)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(raw), Valid: true}, nil
}

// DecodePinnedProviders reads what EncodePinnedProviders stored. A value that
// is not a JSON array of strings is an error, so the lease is refused rather
// than read as unpinned.
func DecodePinnedProviders(v sql.NullString) ([]string, error) {
	if !v.Valid {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(v.String), &out); err != nil {
		return nil, fmt.Errorf("lease pinned providers: %w", err)
	}
	if out == nil {
		return nil, errors.New("lease pinned providers: not a JSON array")
	}
	return out, nil
}
