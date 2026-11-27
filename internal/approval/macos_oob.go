package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MacOSDialogApprover triggers a native macOS modal confirmation dialog over a parent-child pipe.
// Any cancel, timeout, or non-macOS host fails closed.
type MacOSDialogApprover struct {
	Timeout time.Duration
}

func (m *MacOSDialogApprover) Confirm(ctx context.Context, req ConfirmationRequest) (Proof, error) {
	if runtime.GOOS != "darwin" {
		return Proof{}, errors.New("macos native dialog approver is only supported on darwin")
	}
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	msg := confirmationMessage(req)

	// Pass the message as an explicit argv argument to osascript (no shell interpolation).
	script := `on run argv
	set msg to item 1 of argv
	set res to display dialog msg with title "Sablecourt Approval" buttons {"Deny", "Approve"} default button "Deny" cancel button "Deny"
	return button returned of res
end run`

	cmd := exec.CommandContext(cctx, "/usr/bin/osascript", "-e", script, "--", msg)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return Proof{}, fmt.Errorf("dialog denied or failed: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	if strings.TrimSpace(stdout.String()) != "Approve" {
		return Proof{}, errors.New("operator did not click Approve")
	}
	return Proof{
		Method:      "macos_osascript_dialog",
		ConfirmedAt: time.Now().UTC(),
		Digest:      req.PayloadSHA256,
	}, nil
}

// targetFields lists, per capability, the payload fields that say where an
// action lands. The dialog shows them first and never shortens them.
var targetFields = map[string][]string{
	"action:git.open_pr":               {"owner", "repo", "head", "base"},
	"action:git.push":                  {"branch", "commit_sha", "destination"},
	"action:git.merge_pr":              {"owner", "repo", "pr_number", "commit_sha"},
	"action:git.create_branch":         {"branch", "base"},
	"action:sandbox.propose_promotion": {"workload", "branch"},
}

const (
	freeTextLimit  = 160
	otherFieldsMax = 12
)

// confirmationMessage builds the text the operator reads in the dialog: the
// action's target fields in full, the full payload digest, then the remaining
// fields with long text shortened. Every value is quoted so a newline in the
// payload cannot fake a line of the dialog.
func confirmationMessage(req ConfirmationRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Sablecourt Approval Request\n\nWorkload: %s\nCapability: %s\nTask: %s\nApproval: %s\n",
		strconv.Quote(req.WorkloadName), strconv.Quote(req.Capability), strconv.Quote(req.TaskID), strconv.Quote(req.ApprovalID))

	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(req.Summary), &fields); err != nil || fields == nil {
		fmt.Fprintf(&b, "\nDigest (SHA-256): %s\n\nPayload: %s\n", req.PayloadSHA256, shorten(req.Summary, 4*freeTextLimit))
		return b.String()
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	targets := targetFields[req.Capability]
	isTarget := func(k string) bool {
		for _, t := range targets {
			// The payload is the gate's own encoding, whose keys are the exact field names; the case-insensitive match is a safeguard so a key of another case is still shown as a target.
			if strings.EqualFold(k, t) {
				return true
			}
		}
		return false
	}
	if len(targets) > 0 {
		b.WriteString("\nTarget:\n")
		for _, t := range targets {
			found := false
			for _, k := range keys {
				if strings.EqualFold(k, t) {
					fmt.Fprintf(&b, "%s: %s\n", k, fieldValue(fields[k], -1))
					found = true
				}
			}
			if !found {
				fmt.Fprintf(&b, "%s: (missing)\n", t)
			}
		}
	}
	fmt.Fprintf(&b, "\nDigest (SHA-256): %s\n", req.PayloadSHA256)

	shown := 0
	var other []string
	for _, k := range keys {
		if !isTarget(k) {
			other = append(other, k)
		}
	}
	if len(other) > 0 {
		b.WriteString("\nOther fields (long text shortened):\n")
		for _, k := range other {
			if shown == otherFieldsMax {
				fmt.Fprintf(&b, "(%d more fields)\n", len(other)-shown)
				break
			}
			fmt.Fprintf(&b, "%s: %s\n", strconv.Quote(shorten(k, 64)), fieldValue(fields[k], freeTextLimit))
			shown++
		}
	}
	return b.String()
}

// fieldValue renders one payload value quoted; limit < 0 keeps it whole.
func fieldValue(raw json.RawMessage, limit int) string {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		text = string(raw)
	}
	if limit < 0 {
		return strconv.Quote(text)
	}
	out := strconv.Quote(shorten(text, limit))
	if len(text) > limit {
		out += fmt.Sprintf(" (%d bytes in all)", len(text))
	}
	return out
}

func shorten(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
