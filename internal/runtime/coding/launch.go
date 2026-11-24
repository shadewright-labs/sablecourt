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

const maxLaunchInput = 64 * 1024

var modelName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,127}$`)

type launchInput struct {
	prompt, context, model, conclaveID, role string
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
	if len(input.prompt) > maxLaunchInput || len(input.context) > maxLaunchInput {
		return input, errors.New("redacted coding input is oversized")
	}
	return input, nil
}

func (input launchInput) args(harness string) []string {
	if harness == "codex" {
		args := []string{"exec"}
		if input.model != "" {
			args = append(args, "-m", input.model)
		}
		return append(args, input.prompt)
	}
	var args []string
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
	args := input.args(cfg.Harness)
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
	cmd.Dir, cmd.Env = dir, strippedEnv(spec, scratch)
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
	return strippedEnv(spec, home)
}

func strippedEnv(spec scruntime.LaunchSpec, home string) []string {
	env := []string{
		"PATH=/Library/Developer/CommandLineTools/usr/bin:/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin",
		"LANG=en_US.UTF-8",
		"HOME=" + home,
		"TMPDIR=" + home,
		"SABLECOURT_MANAGED=1",
		"SABLECOURT_TASK_ID=" + spec.TaskID,
		"SABLECOURT_WORKLOAD=" + spec.Workload.Metadata.Name,
	}
	if spec.DataGateURL != "" && spec.TaskToken != "" {
		env = append(env, "SABLECOURT_DATA_GATE_URL="+spec.DataGateURL)
	}
	if spec.TaskToken != "" {
		env = append(env, "SABLECOURT_TASK_TOKEN="+spec.TaskToken)
	}
	return env
}
