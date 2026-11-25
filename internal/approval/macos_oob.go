package approval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
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

// confirmationMessage builds the text the operator reads in the dialog.
func confirmationMessage(req ConfirmationRequest) string {
	shortDigest := req.PayloadSHA256
	if len(shortDigest) > 16 {
		shortDigest = shortDigest[:16]
	}
	summary := req.Summary
	if len(summary) > 220 {
		summary = summary[:220] + "..."
	}
	return fmt.Sprintf(
		"Sablecourt Approval Request\n\nWorkload: %s\nCapability: %s\nTask: %s\nDigest: %s\nPayload: %s",
		req.WorkloadName, req.Capability, req.TaskID, shortDigest, summary,
	)
}
