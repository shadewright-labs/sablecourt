package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/contextpack"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/harnessimages"
	"github.com/shadewright-labs/sablecourt/internal/operatortoken"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/runtime/coding"
	"github.com/shadewright-labs/sablecourt/internal/runtime/docker"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const (
	maxCouncilOpenBody = 1 << 20
	// The packet also carries framing and review excerpts.
	maxCouncilQuestion = 30 << 10
	// Reserve encoded headroom for peer excerpts in later rounds inside the 64 KiB launch input.
	maxEncodedOpeningPacket = contextpack.MaxCouncilOpeningEncodedPacketBytes
)

var councilPanel = [...]string{"claude", "codex", "agy"}

type councilOpenRequest struct {
	title, question, repo, ref, writer string
	live                               bool
	spec                               store.CouncilSpec
}

func decodeCouncilOpen(raw []byte, operatorToken string) (councilOpenRequest, error) {
	var req councilOpenRequest
	redact := func(text string) string {
		if operatorToken != "" {
			text = strings.ReplaceAll(text, operatorToken, "[REDACTED:sablecourt_token]")
		}
		return store.RedactCouncilAnswer(text)
	}
	var fields map[string]json.RawMessage
	invalidJSON := errors.New("invalid council JSON object or field")
	if err := jsonv2.Unmarshal(raw, &fields); err != nil || fields == nil {
		return req, invalidJSON
	}
	var decree, writer string
	var live bool
	var roles []string
	var durationSeconds int64
	dest := map[string]any{
		"title": &req.title, "decree": &decree, "question": &req.question,
		"repo": &req.repo, "ref": &req.ref, "live": &live, "writer": &writer,
		"rounds": &req.spec.MaxRounds, "starts": &req.spec.MaxStarts, "durationSeconds": &durationSeconds,
		"with": &roles, "sequential": &req.spec.Sequential,
	}
	for key, value := range fields {
		field, ok := dest[key]
		if !ok || string(value) == "null" || jsonv2.Unmarshal(value, field) != nil {
			return req, invalidJSON
		}
	}

	if len(req.question) > maxCouncilQuestion || strings.ContainsRune(req.question, 0) {
		return req, errors.New("invalid council question or question exceeds packet budget")
	}
	req.question = strings.TrimSpace(req.question)
	if req.question == "" {
		return req, errors.New("council question is required")
	}
	// The client mirrors the question in decree. It is already included in each turn.
	if fields["decree"] != nil && strings.TrimSpace(decree) != req.question {
		return req, errors.New("opening decree must match the council question")
	}
	req.spec.Question = redact(req.question)
	if len(req.spec.Question) > maxCouncilQuestion {
		return req, errors.New("council question exceeds packet budget after redaction")
	}
	packet, err := contextpack.AssembleCouncil("", req.spec.Question, nil, nil)
	if err != nil {
		return req, errors.New("council question cannot be packed")
	}
	encoded, err := json.Marshal(packet.Markdown)
	if err != nil || len(encoded) > maxEncodedOpeningPacket {
		return req, errors.New("council question exceeds launch input budget")
	}
	if len(req.title) > 512 || strings.ContainsRune(req.title, 0) {
		return req, errors.New("invalid council title")
	}
	req.title = redact(strings.TrimSpace(req.title))
	if req.title == "" {
		req.title = "Council"
	}

	hasRef := fields["ref"] != nil
	hasLive := fields["live"] != nil
	hasWriter := fields["writer"] != nil

	req.repo = strings.TrimSpace(req.repo)
	if len(req.repo) > 4096 || !filepath.IsAbs(req.repo) || strings.ContainsRune(req.repo, 0) ||
		redact(req.repo) != req.repo {
		return req, errors.New("council repo must be a valid absolute directory")
	}
	req.ref = strings.TrimSpace(req.ref)
	req.writer = strings.TrimSpace(writer)
	if live && hasRef {
		return req, errors.New("council ref and live checkout cannot be combined")
	}
	if live && hasWriter {
		return req, errors.New("council writer and live checkout cannot be combined")
	}
	if hasLive && !live && !hasRef && !hasWriter {
		return req, errors.New("a valid council ref or live checkout is required")
	}
	if hasRef {
		if len(req.ref) == 0 || len(req.ref) > 256 || strings.HasPrefix(req.ref, "-") ||
			redact(req.ref) != req.ref {
			return req, errors.New("a valid council ref is required")
		}
	}
	if hasWriter {
		if !slices.Contains(councilPanel[:], req.writer) {
			return req, errors.New("council writer must be one of claude, codex, or agy")
		}
		if !hasRef {
			req.ref = "HEAD"
		}
	}
	req.live = live || (!hasRef && !hasWriter)

	if n := req.spec.MaxRounds; n != nil && *n < 1 {
		return req, errors.New("rounds must be a positive integer")
	}
	if n := req.spec.MaxStarts; n != nil && (*n < 1 || *n > domain.ConclaveRunCeiling) {
		return req, errors.New("starts must be between one and six")
	}
	if fields["durationSeconds"] != nil {
		if durationSeconds < 1 || durationSeconds > math.MaxInt64/int64(time.Second) {
			return req, errors.New("durationSeconds must be positive and fit in a duration")
		}
		duration := time.Duration(durationSeconds) * time.Second
		req.spec.Duration = &duration
	}

	if fields["with"] == nil {
		roles = councilPanel[:]
	}
	seen := make(map[string]bool, len(councilPanel))
	if len(roles) != len(councilPanel) {
		return req, errors.New("council panel requires claude, codex, and agy")
	}
	for _, role := range roles {
		if seen[role] || !slices.Contains(councilPanel[:], role) {
			return req, errors.New("council panel requires claude, codex, and agy once each")
		}
		seen[role] = true
		req.spec.Members = append(req.spec.Members, store.CouncilMember{
			Vassal: domain.ConclaveVassal{Role: role, Workload: role, CanMessage: []string{"all"}},
			Zone:   domain.ZoneSandbox,
		})
	}
	return req, nil
}

func parseCouncilIdempotencyKey(r *http.Request, operatorToken string) (string, error) {
	vals := r.Header.Values("Idempotency-Key")
	if len(vals) == 0 {
		return "", nil
	}
	if len(vals) > 1 {
		return "", errors.New("multiple Idempotency-Key headers are not allowed")
	}
	key := strings.TrimSpace(vals[0])
	if key == "" || len(key) > 256 {
		return "", errors.New("invalid Idempotency-Key")
	}
	for _, ch := range key {
		if ch < 0x20 || ch == 0x7f {
			return "", errors.New("invalid Idempotency-Key")
		}
	}
	if (operatorToken != "" && strings.Contains(key, operatorToken)) || store.RedactCouncilAnswer(key) != key {
		return "", errors.New("invalid Idempotency-Key")
	}
	return key, nil
}

func councilOpenDigest(req councilOpenRequest) string {
	roles := make([]string, 0, len(req.spec.Members))
	for _, m := range req.spec.Members {
		roles = append(roles, m.Vassal.Role)
	}
	canonical := struct {
		Title      string         `json:"title"`
		Question   string         `json:"question"`
		Repo       string         `json:"repo"`
		Ref        string         `json:"ref"`
		Live       bool           `json:"live"`
		Writer     string         `json:"writer"`
		MaxRounds  *int           `json:"maxRounds"`
		MaxStarts  *int           `json:"maxStarts"`
		Duration   *time.Duration `json:"duration"`
		Sequential bool           `json:"sequential"`
		Roles      []string       `json:"roles"`
	}{
		Title:      req.title,
		Question:   req.spec.Question,
		Repo:       req.repo,
		Ref:        req.ref,
		Live:       req.live,
		Writer:     req.writer,
		MaxRounds:  req.spec.MaxRounds,
		MaxStarts:  req.spec.MaxStarts,
		Duration:   req.spec.Duration,
		Sequential: req.spec.Sequential,
		Roles:      roles,
	}
	raw, _ := json.Marshal(canonical)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func councilOpen(st store.Store, cfg handlerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fail := func(status int, message string) {
			writeJSON(w, status, map[string]string{"error": message})
		}
		idemKey, err := parseCouncilIdempotencyKey(r, cfg.operatorToken)
		if err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCouncilOpenBody))
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(http.StatusRequestEntityTooLarge, "council request is too large")
			return
		}
		if err != nil {
			fail(http.StatusBadRequest, "cannot read council request")
			return
		}
		req, err := decodeCouncilOpen(raw, cfg.operatorToken)
		if err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		if cfg.councilsDisabled {
			fail(http.StatusServiceUnavailable, "council opening requires Sanctum")
			return
		}
		councils, err := store.Councils(st)
		if err != nil {
			fail(http.StatusServiceUnavailable, "council storage is unavailable")
			return
		}
		var payloadDigest string
		if idemKey != "" {
			payloadDigest = councilOpenDigest(req)
			existing, savedDigest, lookupErr := councils.CouncilByIdempotencyKey(r.Context(), idemKey)
			if lookupErr == nil {
				if savedDigest != payloadDigest {
					fail(http.StatusConflict, "idempotency key already used with a different council request")
					return
				}
				writeJSON(w, http.StatusOK, existing)
				return
			}
			if !errors.Is(lookupErr, store.ErrNotFound) {
				fail(http.StatusInternalServerError, "cannot check council idempotency")
				return
			}
		}
		// Seats run in harness containers; with none and host mode off the
		// dispatcher cannot launch them, so say why before any host check.
		if cfg.codingRuntime != nil {
			if st := cfg.codingRuntime(); st.Mode == "unavailable" {
				fail(http.StatusServiceUnavailable, "council seats cannot start: "+st.unavailableReason())
				return
			}
		}
		if cfg.preflightCouncilHarness == nil {
			for _, role := range councilPanel {
				bin, err := exec.LookPath(role)
				if err != nil || !filepath.IsAbs(bin) {
					fail(http.StatusServiceUnavailable, "required council executable unavailable: "+role)
					return
				}
			}
		}

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		req.spec.Checkout, err = preflightCouncilCheckout(ctx, req.repo, req.ref, req.writer, req.live, cfg.operatorToken, cfg.councilPrivateDirs)
		if err != nil {
			if ctx.Err() != nil {
				fail(http.StatusServiceUnavailable, "council checkout preflight timed out")
				return
			}
			fail(http.StatusBadRequest, err.Error())
			return
		}
		if cfg.onCouncilCheckout != nil {
			cfg.onCouncilCheckout(cancel)
		}
		if cfg.preflightCouncilHarness != nil {
			// Container mode: the container is the confinement. Each role is
			// checked in order, and the first failure refuses the request before
			// anything is reserved.
			// Admission gets its own deadline, independent of the checkout one.
			admit, acancel := context.WithTimeout(r.Context(), 45*time.Second)
			defer acancel()
			ctx = admit
			for _, role := range councilPanel {
				if err := cfg.preflightCouncilHarness(ctx, role); err != nil {
					status, message := councilHarnessRefusal(role, err)
					fail(status, message)
					return
				}
			}
		} else {
			checkSandbox := cfg.checkCouncilSandbox
			if checkSandbox == nil {
				checkSandbox = coding.CheckSandbox
			}
			if checkSandbox(ctx) != nil {
				fail(http.StatusServiceUnavailable, "council checkout confinement is unavailable")
				return
			}
		}
		// The dispatcher must be able to launch this pinned, confined panel.
		if cfg.checkCouncilRuntime == nil || cfg.checkCouncilRuntime(ctx, req.spec.Checkout) != nil || ctx.Err() != nil {
			fail(http.StatusServiceUnavailable, "confined council dispatch is unavailable")
			return
		}

		now := time.Now().UTC()
		room := domain.ConclaveSession{
			ID: "ccl_" + rand.Text(), Title: req.title, State: domain.ConclaveStateActive,
			CreatedAt: now, UpdatedAt: now,
		}
		req.spec.IdempotencyKey = idemKey
		req.spec.RequestSHA256 = payloadDigest
		if err := councils.CreateCouncil(r.Context(), room, req.spec); err != nil {
			if errors.Is(err, store.ErrConflict) && idemKey != "" {
				existing, savedDigest, lookupErr := councils.CouncilByIdempotencyKey(r.Context(), idemKey)
				if lookupErr == nil {
					if savedDigest != payloadDigest {
						fail(http.StatusConflict, "idempotency key already used with a different council request")
						return
					}
					writeJSON(w, http.StatusOK, existing)
					return
				}
			}
			fail(http.StatusInternalServerError, "cannot persist council")
			return
		}
		writeJSON(w, http.StatusCreated, room)
	}
}

var homeCredentialPaths = [...]string{
	".ssh",
	".gnupg",
	".aws",
	".kube",
	".docker",
	".config",
	".azure",
	".gcloud",
	".gcp",
	".claude",
	".codex",
	".gemini",
	".agy",
	".netrc",
	".npmrc",
	".pypirc",
	".git-credentials",
	filepath.Join(".local", "share", "sablecourt"),
	filepath.Join("Library", "Keychains"),
	filepath.Join("Library", "Application Support"),
}

func ownedDirectoryIdentity(info os.FileInfo, euid uint32) (uint64, uint64, bool) {
	if !info.IsDir() {
		return 0, 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != euid {
		return 0, 0, false
	}
	return uint64(stat.Dev), stat.Ino, true
}

func resolveExistingPath(path string) (string, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return "", errors.New("path must be absolute")
	}
	cur := clean
	var tail []string
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrPermission) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return clean, nil
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

func pathWithinDir(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

func pathOverlaps(a, b string) bool {
	return pathWithinDir(a, b) || pathWithinDir(b, a)
}

func forbiddenCouncilPaths(extra []string) ([]string, error) {
	stateDir, err := operatortoken.StateDir()
	if err != nil {
		return nil, errors.New("cannot resolve daemon state directory")
	}
	homeDir, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(homeDir) {
		return nil, errors.New("cannot resolve daemon home directory")
	}
	raw := make([]string, 0, 1+len(extra)+len(homeCredentialPaths))
	raw = append(raw, stateDir)
	raw = append(raw, extra...)
	for _, rel := range homeCredentialPaths {
		raw = append(raw, filepath.Join(homeDir, rel))
	}
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		resolved, err := resolveExistingPath(p)
		if err != nil {
			return nil, errors.New("cannot resolve protected daemon path")
		}
		out = append(out, resolved)
	}
	return out, nil
}

func preflightCouncilCheckout(ctx context.Context, repo, ref, writer string, live bool, operatorToken string, privateDirs []string) (store.CouncilCheckout, error) {
	checkout := store.CouncilCheckout{}
	source, err := filepath.EvalSymlinks(repo)
	if err != nil || !filepath.IsAbs(source) || operatorToken != "" && strings.Contains(source, operatorToken) ||
		store.RedactCouncilAnswer(source) != source {
		return checkout, errors.New("council repo is unavailable")
	}
	euid := uint32(os.Geteuid()) // #nosec G115 -- os.Geteuid returns the kernel uid_t used by ownedDirectoryIdentity to compare directory UIDs.
	sourceInfo, err := os.Lstat(source)
	if err != nil || !sourceInfo.IsDir() {
		return checkout, errors.New("council repo is unavailable")
	}
	sourceDev, sourceIno, ok := ownedDirectoryIdentity(sourceInfo, euid)
	if !ok {
		return checkout, errors.New("council repo must be owned by the daemon user")
	}
	forbidden, err := forbiddenCouncilPaths(privateDirs)
	if err != nil {
		return checkout, err
	}
	for _, p := range forbidden {
		if pathOverlaps(source, p) {
			return checkout, errors.New("council repo must not overlap daemon state or credential directories")
		}
	}
	gitDir := filepath.Join(source, ".git")
	gitInfo, err := os.Lstat(gitDir)
	if err != nil || !gitInfo.IsDir() {
		return checkout, errors.New("council repo must be a primary Git checkout")
	}
	if _, _, ok := ownedDirectoryIdentity(gitInfo, euid); !ok {
		return checkout, errors.New("council repo Git storage must be owned by the daemon user")
	}
	if _, err := os.Lstat(filepath.Join(gitDir, "commondir")); !errors.Is(err, os.ErrNotExist) {
		return checkout, errors.New("council repo must have its own Git storage")
	}
	git := func(args ...string) (string, error) {
		flags := []string{"-C", source, "--git-dir=" + gitDir, "--work-tree=" + source, "--no-replace-objects",
			"-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull}
		cmd := exec.CommandContext(ctx, "git", append(flags, args...)...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL="}
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	list, err := git("worktree", "list", "--porcelain", "-z")
	if err != nil {
		return checkout, errors.New("council repo must be a primary Git checkout")
	}
	primary, _, _ := strings.Cut(list, "\x00\x00")
	primaryFields := strings.Split(primary, "\x00")
	if len(primaryFields) < 2 || primaryFields[0] != "worktree "+source || !strings.HasPrefix(primaryFields[1], "HEAD ") {
		return checkout, errors.New("council repo must be a primary Git checkout")
	}
	commitFor := func(name string) string {
		id, err := git("rev-parse", "--verify", "--end-of-options", name+"^{commit}")
		if err != nil || !councilCommitID(id) {
			return ""
		}
		return id
	}
	if live {
		if commitFor("HEAD") == "" {
			return checkout, errors.New("council repo HEAD must identify a commit")
		}
		if curInfo, err := os.Lstat(source); err != nil || !os.SameFile(sourceInfo, curInfo) {
			return checkout, errors.New("council repo changed during admission")
		}
		return store.CouncilCheckout{Source: source, SourceDev: sourceDev, SourceIno: sourceIno}, nil
	}
	badRef := errors.New("council ref must identify a branch, tag, HEAD, or full commit ID")
	if _, err := git("check-ref-format", "--allow-onelevel", ref); err != nil {
		return checkout, badRef
	}
	exactRefCommit := func(name string) string {
		id, err := git("show-ref", "--verify", "--hash", "--", name)
		if err != nil || !councilCommitID(id) {
			return ""
		}
		return commitFor(id)
	}
	var commit string
	switch {
	case councilCommitID(ref):
		if id := commitFor(ref); strings.EqualFold(id, ref) {
			commit = id
		}
	case ref == "HEAD":
		commit = commitFor(ref)
	case strings.HasPrefix(ref, "refs/heads/"), strings.HasPrefix(ref, "refs/tags/"):
		commit = exactRefCommit(ref)
	case !strings.HasPrefix(ref, "refs/"):
		head := exactRefCommit("refs/heads/" + ref)
		tag := exactRefCommit("refs/tags/" + ref)
		if head != "" && tag != "" && head != tag {
			return checkout, errors.New("council ref is ambiguous between a branch and tag")
		}
		commit = head
		if commit == "" {
			commit = tag
		}
	}
	if commit == "" {
		return checkout, badRef
	}
	if curInfo, err := os.Lstat(source); err != nil || !os.SameFile(sourceInfo, curInfo) {
		return checkout, errors.New("council repo changed during admission")
	}
	return store.CouncilCheckout{Source: source, SourceDev: sourceDev, SourceIno: sourceIno, Ref: ref, Commit: commit, WriterRole: writer}, nil
}

// councilSandbox is the sandbox kind recorded on the built-in panel workloads.
func councilSandbox(container bool) string {
	if container {
		return "container"
	}
	return "seatbelt"
}

// councilHarnessRefusal turns a harness preflight failure into an HTTP status
// and a message that names the missing piece and the command that fixes it.
// A backend failure never suggests a command: nothing is known to be missing.
func councilHarnessRefusal(role string, err error) (int, string) {
	var he *docker.HarnessError
	if ctxErr := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled); ctxErr {
		return http.StatusServiceUnavailable, "council harness preflight timed out or was cancelled: " + role
	}
	if !errors.As(err, &he) {
		return http.StatusServiceUnavailable, "council harness preflight failed: " + role
	}
	switch he.Fault {
	case docker.FaultImageMissing:
		return http.StatusPreconditionFailed, "council image for " + role + " is missing: run " + harnessimages.BuildCommand(role)
	case docker.FaultVolumeMissing, docker.FaultLoginMissing:
		return http.StatusPreconditionFailed, "council login for " + role + " is missing: run " + harnessimages.LoginCommand(role)
	case docker.FaultToolMissing:
		return http.StatusPreconditionFailed, "council tool for " + role + " is not installed: run " + harnessimages.InstallCommand(role)
	case docker.FaultToolVersion:
		return http.StatusPreconditionFailed, "council tool for " + role + " is not the pinned version: run " + harnessimages.InstallCommand(role)
	case docker.FaultToolChecksum:
		return http.StatusPreconditionFailed, "council tool for " + role + " does not match its pinned SHA-256: run " + harnessimages.InstallCommand(role)
	case docker.FaultToolInvalid:
		return http.StatusConflict, "council tool volume for " + role + " exists but is not a local Sablecourt tool volume for this harness (driver, options or labels differ): remove it by hand, then run " + harnessimages.InstallCommand(role)
	case docker.FaultVolumeInvalid:
		return http.StatusConflict, "council login volume for " + role + " exists but is not a local Sablecourt volume for this harness (driver, options or labels differ): remove it by hand, then run " + harnessimages.LoginCommand(role)
	default:
		return http.StatusServiceUnavailable, "council harness runtime failed for " + role + ": Docker or the harness configuration is unavailable"
	}
}

func ensureCouncilPanelWorkloads(ctx context.Context, st store.Store, b *config.Bundle, sandbox string) (*config.Bundle, error) {
	if b == nil {
		b = &config.Bundle{}
	}
	if b.Workloads == nil {
		b.Workloads = make(map[string]domain.Workload, len(councilPanel))
	}
	for _, role := range councilPanel {
		if _, exists := b.Workloads[role]; exists {
			return nil, errors.New("workload " + role + ": name is reserved for the built-in council panel")
		}
		w := domain.Workload{
			Metadata: domain.ObjectMeta{Name: role, Zone: domain.ZoneSandbox},
			Spec: domain.WorkloadSpec{
				Class:  domain.ClassCodingAgent,
				Coding: &domain.CodingConfig{Harness: role, Sandbox: sandbox},
			},
		}
		if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{
			Name:  w.Metadata.Name,
			Zone:  w.Metadata.Zone,
			Class: w.Spec.Class,
		}); err != nil {
			return nil, err
		}
		b.Workloads[role] = w
	}
	return b, nil
}

func councilRuntimeChecker(getBundle func() *config.Bundle, adapters map[domain.WorkloadClass]scruntime.Adapter, sandbox string) func(context.Context, store.CouncilCheckout) error {
	return func(_ context.Context, checkout store.CouncilCheckout) error {
		if adapters == nil || adapters[domain.ClassCodingAgent] == nil {
			return errors.New("coding adapter unavailable")
		}
		if getBundle == nil {
			return errors.New("workload bundle unavailable")
		}
		b := getBundle()
		if b == nil {
			return errors.New("workload bundle unavailable")
		}
		for _, role := range councilPanel {
			wl, ok := b.Workloads[role]
			if !ok || wl.Metadata.Zone != domain.ZoneSandbox || wl.Spec.Class != domain.ClassCodingAgent ||
				wl.Spec.Coding == nil || wl.Spec.Coding.Harness != role || wl.Spec.Coding.Sandbox != sandbox ||
				wl.Spec.Grants.ModelGate.Allowed || len(wl.Spec.Grants.MCPTools) > 0 ||
				len(wl.Spec.Grants.MCPServers) > 0 || len(wl.Spec.Grants.Actions) > 0 ||
				len(wl.Spec.Grants.DelegateTo) > 0 {
				return errors.New("council panel workload unavailable: " + role)
			}
			if wl.Spec.Coding.RepoPath != "" && checkout.Source != "" &&
				!sameCheckoutSource(checkout.Source, wl.Spec.Coding.RepoPath) {
				return errors.New("council panel workload repo mismatch: " + role)
			}
		}
		return nil
	}
}

func sameCheckoutSource(a, b string) bool {
	if a == b {
		return true
	}
	if a == "" || b == "" {
		return false
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	ra, errA := filepath.EvalSymlinks(absA)
	rb, errB := filepath.EvalSymlinks(absB)
	return errA == nil && errB == nil && ra == rb
}

func councilCommitID(id string) bool {
	if len(id) != 40 && len(id) != 64 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
