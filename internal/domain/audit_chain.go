package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Audit row hash versions. Version 1 rows were written before the chain hashed
// the row ID and sequence; migration 0016 (SQLite) and 0012 (Postgres) mark every
// existing row as version 1. Every row appended after that is version 2.
const (
	AuditHashV1 = 1
	AuditHashV2 = 2
)

// AuditGenesis is the previous hash of the first row of the chain.
const AuditGenesis = "genesis"

// auditHashV2Domain separates version 2 row hashes from any other SHA-256 input.
const auditHashV2Domain = "sablecourt-audit-row-v2"

// ErrAuditChainBroken marks a verification failure caused by the chain contents
// or by an anchor the chain does not reach, as opposed to a read error.
var ErrAuditChainBroken = errors.New("audit chain broken")

// ComputeAuditRowHash returns the version 2 hash of an audit row: SHA-256 over a
// domain tag followed by each field as an 8-byte big-endian length and its bytes,
// in this order: tag, row ID, sequence, previous hash, category, task ID,
// workload, action, outcome, detail JSON, creation time. The length prefix makes
// the encoding unambiguous, so moving bytes between fields changes the hash.
// rec.PrevHash, rec.RowHash, rec.HashVersion and rec.CreatedAt are ignored.
func ComputeAuditRowHash(prevHash string, rec AuditRecord, createdAtRFC3339 string) string {
	h := sha256.New()
	var n [8]byte
	for _, field := range []string{
		auditHashV2Domain,
		strconv.FormatInt(rec.ID, 10),
		strconv.FormatInt(rec.Seq, 10),
		prevHash,
		rec.Category,
		rec.TaskID,
		rec.WorkloadName,
		rec.Action,
		rec.Outcome,
		rec.DetailJSON,
		createdAtRFC3339,
	} {
		binary.BigEndian.PutUint64(n[:], uint64(len(field)))
		h.Write(n[:])
		h.Write([]byte(field))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ComputeAuditRowHashV1 returns the version 1 hash kept for rows written before
// version 2. It joins fields with an unescaped '|' and leaves out the row ID and
// sequence, so it cannot detect a '|' moved between adjacent fields or an edited
// row ID. It is used only to verify those older rows.
func ComputeAuditRowHashV1(prevHash string, rec AuditRecord, createdAtRFC3339 string) string {
	if prevHash == "" {
		prevHash = AuditGenesis
	}
	payload := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s",
		prevHash, rec.Category, rec.TaskID, rec.WorkloadName, rec.Action, rec.Outcome, rec.DetailJSON, createdAtRFC3339)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// AuditAnchor is a chain head an operator recorded earlier from a verify result.
// Verifying against it fails when the chain no longer reaches Seq or when the row
// at Seq has another hash, which catches deletion of the newest rows and a chain
// recomputed after an edit at or before Seq.
type AuditAnchor struct {
	Seq  int64
	Hash string
}

// ParseAuditAnchor parses "SEQ:HASH", where SEQ is a positive sequence number and
// HASH a lowercase hex SHA-256 digest. An empty string means no anchor.
func ParseAuditAnchor(s string) (*AuditAnchor, error) {
	if s == "" {
		return nil, nil
	}
	seqText, hash, ok := strings.Cut(s, ":")
	if !ok {
		return nil, fmt.Errorf("audit anchor %q: want SEQ:HASH", s)
	}
	seq, err := strconv.ParseInt(seqText, 10, 64)
	if err != nil || seq < 1 || strconv.FormatInt(seq, 10) != seqText {
		return nil, fmt.Errorf("audit anchor %q: sequence must be a positive integer", s)
	}
	if !isLowerHexSHA256(hash) {
		return nil, fmt.Errorf("audit anchor %q: hash must be 64 lowercase hex characters", s)
	}
	return &AuditAnchor{Seq: seq, Hash: hash}, nil
}

func isLowerHexSHA256(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// AuditChainHead is the result of a successful verification. Seq and Hash name
// the newest row (Seq 0 and Hash "genesis" for an empty chain); an operator keeps
// them outside the database and passes them back as an AuditAnchor later.
type AuditChainHead struct {
	Rows       int
	LegacyRows int
	Seq        int64
	ID         int64
	Hash       string
}

// AuditChainVerifier checks audit rows fed in ID order. Both stores use it so
// the chain rules live in one place.
type AuditChainVerifier struct {
	anchor     *AuditAnchor
	anchorSeen bool
	sawV2      bool
	head       AuditChainHead
}

// NewAuditChainVerifier starts a verification, optionally against an anchor.
func NewAuditChainVerifier(anchor *AuditAnchor) *AuditChainVerifier {
	return &AuditChainVerifier{anchor: anchor, head: AuditChainHead{Hash: AuditGenesis}}
}

// Add checks the next row. createdAt is the stored creation time text.
// It refuses a row with a blank hash, a sequence that is not the next one, a
// previous hash that is not the last row hash, an unknown hash version, a
// version 1 row after a version 2 row, a row hash that does not match, and a
// row at the anchor sequence with another hash.
func (v *AuditChainVerifier) Add(rec AuditRecord, createdAt string) error {
	if rec.RowHash == "" || rec.PrevHash == "" {
		return fmt.Errorf("%w: audit row %d has no stored hash", ErrAuditChainBroken, rec.ID)
	}
	wantSeq := v.head.Seq + 1
	if rec.Seq != wantSeq {
		return fmt.Errorf("%w: audit row %d has sequence %d, expected %d", ErrAuditChainBroken, rec.ID, rec.Seq, wantSeq)
	}
	if rec.PrevHash != v.head.Hash {
		return fmt.Errorf("%w at row %d: prev_hash=%q expected=%q", ErrAuditChainBroken, rec.ID, rec.PrevHash, v.head.Hash)
	}
	var want string
	switch rec.HashVersion {
	case AuditHashV1:
		if v.sawV2 {
			return fmt.Errorf("%w: audit row %d has hash version 1 after a version 2 row", ErrAuditChainBroken, rec.ID)
		}
		want = ComputeAuditRowHashV1(rec.PrevHash, rec, createdAt)
	case AuditHashV2:
		v.sawV2 = true
		want = ComputeAuditRowHash(rec.PrevHash, rec, createdAt)
	default:
		return fmt.Errorf("%w: audit row %d has unknown hash version %d", ErrAuditChainBroken, rec.ID, rec.HashVersion)
	}
	if rec.RowHash != want {
		return fmt.Errorf("%w: audit row %d tampered: row_hash=%q computed=%q", ErrAuditChainBroken, rec.ID, rec.RowHash, want)
	}
	if v.anchor != nil && rec.Seq == v.anchor.Seq {
		if rec.RowHash != v.anchor.Hash {
			return fmt.Errorf("%w: audit row %d at anchor sequence %d has hash %q, anchor says %q", ErrAuditChainBroken, rec.ID, rec.Seq, rec.RowHash, v.anchor.Hash)
		}
		v.anchorSeen = true
	}
	v.head.Rows++
	if rec.HashVersion == AuditHashV1 {
		v.head.LegacyRows++
	}
	v.head.Seq, v.head.ID, v.head.Hash = rec.Seq, rec.ID, rec.RowHash
	return nil
}

// Rows reports how many rows passed so far.
func (v *AuditChainVerifier) Rows() int { return v.head.Rows }

// Finish returns the verified head, or an error when an anchor was given and
// the chain ends before its sequence (rows deleted from the tail).
func (v *AuditChainVerifier) Finish() (AuditChainHead, error) {
	if v.anchor != nil && !v.anchorSeen {
		return v.head, fmt.Errorf("%w: chain ends at sequence %d before anchor sequence %d", ErrAuditChainBroken, v.head.Seq, v.anchor.Seq)
	}
	return v.head, nil
}
