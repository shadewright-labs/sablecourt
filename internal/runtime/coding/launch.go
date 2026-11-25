package coding

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

const (
	maxLaunchInput      = 64 * 1024
	maxCheckoutRefBytes = 256
	maxConclaveIDBytes  = 128
	maxCouncilRoleBytes = 64

	checkoutModeLive   = "live"
	checkoutModeRef    = "ref"
	checkoutModeWriter = "writer"
)

var modelName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,127}$`)

type launchInput struct {
	prompt, context, model, conclaveID, role                    string
	checkoutMode, checkoutRef, checkoutWriter, checkoutSourceID string
}

func (input launchInput) isReadOnly() bool {
	switch input.checkoutMode {
	case checkoutModeLive, checkoutModeRef:
		return true
	case checkoutModeWriter:
		return input.role != input.checkoutWriter
	default:
		return false
	}
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func validCouncilRole(role string) bool {
	return role != "" && len(role) <= maxCouncilRoleBytes && role == strings.TrimSpace(role) &&
		!hasControlChars(role) && !strings.ContainsAny(role, " \t\n\r") &&
		!strings.HasPrefix(role, "@") && role != "operator" && role != "all" && role != "system"
}

func validCheckoutRef(ref string) bool {
	if ref == "" || len(ref) > maxCheckoutRefBytes || ref != strings.TrimSpace(ref) || hasControlChars(ref) {
		return false
	}
	if isCommitID(ref) || ref == "HEAD" {
		return true
	}
	if strings.HasPrefix(ref, "-") || strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") ||
		strings.HasSuffix(ref, ".") || strings.Contains(ref, "..") || strings.Contains(ref, "//") ||
		strings.ContainsAny(ref, " \t\n\r@~^:?*[\\") {
		return false
	}
	if strings.HasPrefix(ref, "refs/") && !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
		return false
	}
	for _, comp := range strings.Split(ref, "/") {
		if comp == "" || comp == "." || comp == ".." || strings.HasPrefix(comp, ".") || strings.HasSuffix(comp, ".lock") {
			return false
		}
	}
	return true
}

func validConclaveID(id string) bool {
	return id != "" && len(id) <= maxConclaveIDBytes && id == strings.TrimSpace(id) && !hasControlChars(id)
}

func parseLaunchInput(raw string) (launchInput, error) {
	var input launchInput
	if len(raw) > maxLaunchInput {
		return input, errors.New("coding input is oversized")
	}
	if raw == "" {
		raw = "{}"
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	if start, err := decoder.Token(); err != nil || start != json.Delim('{') {
		return input, errors.New("coding input must be a JSON object")
	}
	fields := map[string]*string{
		"prompt": &input.prompt, "context_packet": &input.context, "model": &input.model,
		"conclaveId": &input.conclaveID, "role": &input.role,
		"checkoutMode": &input.checkoutMode, "checkoutRef": &input.checkoutRef,
		"checkoutWriter": &input.checkoutWriter, "checkoutSourceId": &input.checkoutSourceID,
	}
	seen := make(map[string]bool)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return input, errors.New("invalid coding input field")
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return input, errors.New("invalid or duplicate coding input field")
		}
		seen[name] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return input, errors.New("invalid coding input value")
		}
		if field := fields[name]; field != nil {
			var str *string
			if err := json.Unmarshal(value, &str); err != nil || str == nil || strings.ContainsRune(*str, 0) {
				return input, errors.New("coding input field must be a string without NUL")
			}
			*field = *str
		}
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return input, errors.New("invalid coding input object")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return input, errors.New("unexpected data after coding input")
	}
	input.model = strings.TrimSpace(input.model)
	if seen["model"] && !modelName.MatchString(input.model) {
		return input, errors.New("invalid coding model name")
	}
	if seen["checkoutMode"] {
		switch input.checkoutMode {
		case checkoutModeLive, checkoutModeRef, checkoutModeWriter:
		default:
			return input, errors.New("invalid coding checkoutMode")
		}
	} else if seen["conclaveId"] && (seen["round"] || seen["attempt"] || !seen["activation"]) {
		return input, errors.New("council task requires explicit checkoutMode")
	}
	if seen["checkoutRef"] {
		if input.checkoutMode != checkoutModeRef && input.checkoutMode != checkoutModeWriter {
			return input, errors.New("checkoutRef is only valid in ref or writer checkout modes")
		}
		if !validCheckoutRef(input.checkoutRef) {
			return input, errors.New("invalid coding checkoutRef")
		}
	}
	if seen["checkoutWriter"] {
		if input.checkoutMode != checkoutModeWriter {
			return input, errors.New("checkoutWriter is only valid in writer checkout mode")
		}
		if !validCouncilRole(input.checkoutWriter) {
			return input, errors.New("invalid coding checkoutWriter")
		}
	}
	switch input.checkoutMode {
	case checkoutModeLive:
		if seen["conclaveId"] && !validConclaveID(input.conclaveID) {
			return input, errors.New("invalid coding conclaveId")
		}
		if seen["role"] && !validCouncilRole(input.role) {
			return input, errors.New("invalid coding role")
		}
	case checkoutModeRef:
		if !seen["checkoutRef"] {
			return input, errors.New("ref checkout mode requires checkoutRef")
		}
		if !validConclaveID(input.conclaveID) {
			return input, errors.New("ref checkout mode requires a valid conclaveId")
		}
		if seen["role"] && !validCouncilRole(input.role) {
			return input, errors.New("invalid coding role")
		}
	case checkoutModeWriter:
		if !seen["checkoutWriter"] {
			return input, errors.New("writer checkout mode requires checkoutWriter")
		}
		if !validConclaveID(input.conclaveID) {
			return input, errors.New("writer checkout mode requires a valid conclaveId")
		}
		if !validCouncilRole(input.role) {
			return input, errors.New("writer checkout mode requires a valid role")
		}
	}
	if strings.TrimSpace(input.context) != "" {
		input.prompt = input.context
	}
	if strings.TrimSpace(input.prompt) == "" {
		input.prompt = raw
	}
	if strings.HasPrefix(strings.TrimSpace(input.prompt), "-") {
		return input, errors.New("coding prompt starts with an option prefix")
	}
	for _, field := range []*string{&input.prompt, &input.context, &input.conclaveID, &input.role} {
		if hasUnfinishedPrivateKey(*field) {
			return input, errors.New("coding input contains an unfinished private key")
		}
		*field = redact.String(*field)
	}
	for _, id := range []string{input.checkoutRef, input.checkoutWriter} {
		if hasUnfinishedPrivateKey(id) {
			return input, errors.New("coding input contains an unfinished private key")
		}
	}
	if len(input.prompt) > maxLaunchInput || len(input.context) > maxLaunchInput {
		return input, errors.New("redacted coding input is oversized")
	}
	return input, nil
}

func (input launchInput) args(harness string, readOnly bool) []string {
	if harness == "codex" {
		args := []string{"exec"}
		if readOnly {
			args = append(args, "--sandbox", "read-only")
		}
		if input.model != "" {
			args = append(args, "-m", input.model)
		}
		return append(args, input.prompt)
	}
	var args []string
	if readOnly {
		if harness == "agy" {
			args = append(args, "--mode", "plan")
		} else {
			args = append(args, "--permission-mode", "plan")
		}
	}
	if input.model != "" {
		args = append(args, "--model", input.model)
	}
	return append(args, "-p", input.prompt)
}

func (a *Adapter) buildCommand(spec scruntime.LaunchSpec, input launchInput, dir, scratch string, state *worktreeState, readOnly bool) (*exec.Cmd, string, error) {
	cfg := spec.Workload.Spec.Coding
	bin, err := resolveHarness(cfg.Harness, a.HarnessPaths[cfg.Harness])
	if err != nil {
		return nil, "", err
	}
	args := input.args(cfg.Harness, readOnly)
	var profile string
	if cfg.Sandbox == "seatbelt" {
		dir, err = resolvedPath(dir)
		if err != nil {
			return nil, "", err
		}
		scratch, err = resolvedPath(scratch)
		if err != nil {
			return nil, "", err
		}
		profile, err = seatbeltProfile(dir, scratch, bin, state, readOnly)
		if err != nil {
			return nil, "", err
		}
		bin, args = seatbeltCommand(profile, bin, args)
	}
	cmd := exec.Command(bin, args...)
	env := strippedEnv(spec, scratch, readOnly)
	if state != nil && state.Mode == checkoutModeLive {
		gitDir, err := resolvedPath(state.GitDir)
		if err != nil {
			return nil, "", err
		}
		workTree, err := resolvedPath(dir)
		if err != nil {
			return nil, "", err
		}
		env = append(env, "GIT_DIR="+gitDir, "GIT_WORK_TREE="+workTree)
	}
	cmd.Dir, cmd.Env = dir, env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, profile, nil
}

func resolveHarness(harness, configured string) (string, error) {
	path := configured
	if path == "" {
		path = harness
	} else if !filepath.IsAbs(path) {
		return "", fmt.Errorf("configured coding harness %q path must be absolute", harness)
	}
	found, err := exec.LookPath(path)
	if err == nil && !filepath.IsAbs(found) {
		err = errors.New("executable resolved to a relative path")
	}
	if err == nil {
		found, err = resolvedPath(found)
	}
	if err != nil {
		return "", fmt.Errorf("resolve coding harness %q at %q: %w", harness, path, err)
	}
	return found, nil
}

// BuildStrippedEnv returns an allowlisted environment with scoped gate credentials.
func BuildStrippedEnv(spec scruntime.LaunchSpec) []string {
	home, _ := os.UserHomeDir()
	return strippedEnv(spec, home, false)
}

func strippedEnv(spec scruntime.LaunchSpec, home string, readOnly bool) []string {
	env := []string{
		"PATH=/Library/Developer/CommandLineTools/usr/bin:/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin",
		"LANG=en_US.UTF-8",
		"HOME=" + home,
		"TMPDIR=" + home,
		"SABLECOURT_MANAGED=1",
		"SABLECOURT_TASK_ID=" + spec.TaskID,
		"SABLECOURT_WORKLOAD=" + spec.Workload.Metadata.Name,
	}
	if readOnly {
		env = append(env, "GIT_OPTIONAL_LOCKS=0")
	}
	if spec.DataGateURL != "" && spec.TaskToken != "" {
		env = append(env, "SABLECOURT_DATA_GATE_URL="+spec.DataGateURL)
	}
	if spec.TaskToken != "" {
		env = append(env, "SABLECOURT_TASK_TOKEN="+spec.TaskToken)
	}
	return env
}
