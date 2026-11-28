package domain

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

type chainRow struct {
	rec     AuditRecord
	created string
}

// buildChain returns rows already sealed: legacy version 1 rows first, then
// version 2 rows, as a migrated store holds them.
func buildChain(legacy, current int) []chainRow {
	var rows []chainRow
	prev := AuditGenesis
	for i := range legacy + current {
		r := chainRow{
			rec: AuditRecord{
				ID: int64(i + 1), Seq: int64(i + 1), Category: "policy", Action: "act", Outcome: "allow",
				DetailJSON: fmt.Sprintf(`{"n":%d}`, i), PrevHash: prev,
			},
			created: fmt.Sprintf("2026-01-01T00:00:%02dZ", i),
		}
		if i < legacy {
			r.rec.HashVersion = AuditHashV1
			r.rec.RowHash = ComputeAuditRowHashV1(prev, r.rec, r.created)
		} else {
			r.rec.HashVersion = AuditHashV2
			r.rec.RowHash = ComputeAuditRowHash(prev, r.rec, r.created)
		}
		prev = r.rec.RowHash
		rows = append(rows, r)
	}
	return rows
}

func verifyRows(rows []chainRow, anchor *AuditAnchor) (AuditChainHead, error) {
	v := NewAuditChainVerifier(anchor)
	for _, r := range rows {
		if err := v.Add(r.rec, r.created); err != nil {
			return AuditChainHead{Rows: v.Rows()}, err
		}
	}
	return v.Finish()
}

// reseal recomputes every hash from index i on, as someone with write access
// to the database but no external anchor could.
func reseal(rows []chainRow, i int) {
	prev := AuditGenesis
	if i > 0 {
		prev = rows[i-1].rec.RowHash
	}
	for ; i < len(rows); i++ {
		rows[i].rec.PrevHash = prev
		if rows[i].rec.HashVersion == AuditHashV1 {
			rows[i].rec.RowHash = ComputeAuditRowHashV1(prev, rows[i].rec, rows[i].created)
		} else {
			rows[i].rec.RowHash = ComputeAuditRowHash(prev, rows[i].rec, rows[i].created)
		}
		prev = rows[i].rec.RowHash
	}
}

func TestAuditChainVerifierRefusals(t *testing.T) {
	clean := buildChain(2, 3)
	head := clean[len(clean)-1].rec
	cases := []struct {
		name    string
		mutate  func([]chainRow) []chainRow
		anchor  *AuditAnchor
		wantErr string
	}{
		{"blank row hash", func(r []chainRow) []chainRow { r[3].rec.RowHash = ""; return r }, nil, "no stored hash"},
		{"blank prev hash", func(r []chainRow) []chainRow { r[3].rec.PrevHash = ""; return r }, nil, "no stored hash"},
		{"blank legacy row hash", func(r []chainRow) []chainRow { r[0].rec.RowHash = ""; return r }, nil, "no stored hash"},
		{"blank every hash", func(r []chainRow) []chainRow {
			for i := range r {
				r[i].rec.RowHash, r[i].rec.PrevHash = "", ""
			}
			return r
		}, nil, "no stored hash"},
		{"edited field", func(r []chainRow) []chainRow { r[3].rec.Outcome = "deny"; return r }, nil, "tampered"},
		{"edited legacy field", func(r []chainRow) []chainRow { r[1].rec.Outcome = "deny"; return r }, nil, "tampered"},
		{"edited row id", func(r []chainRow) []chainRow { r[4].rec.ID = 40; return r }, nil, "tampered"},
		{"pipe moved between fields", func(r []chainRow) []chainRow {
			r[3].rec.Action, r[3].rec.Outcome = "act|allow", ""
			return r
		}, nil, "tampered"},
		{"deleted middle row", func(r []chainRow) []chainRow { return append(r[:2:2], r[3:]...) }, nil, "sequence"},
		{"reordered rows", func(r []chainRow) []chainRow {
			r[2], r[3] = r[3], r[2]
			return r
		}, nil, "sequence"},
		{"edited sequence", func(r []chainRow) []chainRow { r[4].rec.Seq = 9; return r }, nil, "sequence"},
		{"prev hash points elsewhere", func(r []chainRow) []chainRow { r[4].rec.PrevHash = r[2].rec.RowHash; return r }, nil, "prev_hash"},
		{"version 1 after version 2", func(r []chainRow) []chainRow {
			r[4].rec.HashVersion = AuditHashV1
			reseal(r, 4)
			return r
		}, nil, "version 1 after"},
		{"unknown version", func(r []chainRow) []chainRow { r[4].rec.HashVersion = 0; return r }, nil, "unknown hash version"},
		{"truncated tail with anchor", func(r []chainRow) []chainRow { return r[:4] }, &AuditAnchor{Seq: head.Seq, Hash: head.RowHash}, "before anchor"},
		{"recomputed chain with anchor", func(r []chainRow) []chainRow {
			r[1].rec.Outcome = "deny"
			reseal(r, 1)
			return r
		}, &AuditAnchor{Seq: head.Seq, Hash: head.RowHash}, "anchor says"},
		{"anchor hash differs", func(r []chainRow) []chainRow { return r }, &AuditAnchor{Seq: 2, Hash: strings.Repeat("a", 64)}, "anchor says"},
		{"anchor past the head", func(r []chainRow) []chainRow { return r }, &AuditAnchor{Seq: 6, Hash: head.RowHash}, "before anchor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := tc.mutate(append([]chainRow(nil), clean...))
			_, err := verifyRows(rows, tc.anchor)
			if !errors.Is(err, ErrAuditChainBroken) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("verify = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestAuditChainVerifierAccepts(t *testing.T) {
	clean := buildChain(2, 3)
	last := clean[len(clean)-1].rec
	cases := []struct {
		name       string
		rows       []chainRow
		anchor     *AuditAnchor
		wantRows   int
		wantLegacy int
		wantSeq    int64
		wantHash   string
	}{
		{"empty chain", nil, nil, 0, 0, 0, AuditGenesis},
		{"legacy then current", clean, nil, 5, 2, 5, last.RowHash},
		{"current only", buildChain(0, 2), nil, 2, 0, 2, buildChain(0, 2)[1].rec.RowHash},
		{"anchor at head", clean, &AuditAnchor{Seq: 5, Hash: last.RowHash}, 5, 2, 5, last.RowHash},
		{"anchor inside the chain", clean, &AuditAnchor{Seq: 2, Hash: clean[1].rec.RowHash}, 5, 2, 5, last.RowHash},
		// Without an anchor a dropped tail is not visible: the known limit.
		{"truncated tail without anchor", clean[:4], nil, 4, 2, 4, clean[3].rec.RowHash},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := verifyRows(tc.rows, tc.anchor)
			if err != nil {
				t.Fatal(err)
			}
			if got.Rows != tc.wantRows || got.LegacyRows != tc.wantLegacy || got.Seq != tc.wantSeq || got.Hash != tc.wantHash {
				t.Fatalf("head = %+v", got)
			}
		})
	}
}

func TestParseAuditAnchor(t *testing.T) {
	hash := strings.Repeat("0f", 32)
	cases := []struct {
		in      string
		want    *AuditAnchor
		wantErr bool
	}{
		{"", nil, false},
		{"12:" + hash, &AuditAnchor{Seq: 12, Hash: hash}, false},
		{"12", nil, true},
		{":" + hash, nil, true},
		{"0:" + hash, nil, true},
		{"-1:" + hash, nil, true},
		{"+1:" + hash, nil, true},
		{"01:" + hash, nil, true},
		{"1 :" + hash, nil, true},
		{"1:" + strings.ToUpper(hash), nil, true},
		{"1:" + hash[:63], nil, true},
		{"1:" + hash + "0", nil, true},
		{"1:" + hash[:62] + "zz", nil, true},
		{"1:genesis", nil, true},
		{"1:" + hash + ":2", nil, true},
		{"99999999999999999999:" + hash, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseAuditAnchor(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("anchor = %+v, want %+v", got, tc.want)
			}
		})
	}
}
