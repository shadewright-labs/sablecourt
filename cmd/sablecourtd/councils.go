package main

import (
	"context"
	"crypto/rand"
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
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/contextpack"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/runtime/coding"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const (
	maxCouncilOpenBody = 1 << 20
	// The packet also carries framing and review excerpts.
	maxCouncilQuestion = 30 << 10
	// Leave room for the assignment and checkout fields in the adapter's 64 KiB launch input.
	maxEncodedOpeningPacket = 63 << 10
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

func councilOpen(st store.Store, cfg handlerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fail := func(status int, message string) {
			writeJSON(w, status, map[string]string{"error": message})
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
		for _, role := range councilPanel {
			bin, err := exec.LookPath(role)
			if err != nil || !filepath.IsAbs(bin) {
				fail(http.StatusServiceUnavailable, "required council executable unavailable: "+role)
				return
			}
		}

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		req.spec.Checkout, err = preflightCouncilCheckout(ctx, req.repo, req.ref, req.writer, req.live, cfg.operatorToken)
		if err != nil {
			if ctx.Err() != nil {
				fail(http.StatusServiceUnavailable, "council checkout preflight timed out")
				return
			}
			fail(http.StatusBadRequest, err.Error())
			return
		}
		checkSandbox := cfg.checkCouncilSandbox
		if checkSandbox == nil {
			checkSandbox = coding.CheckSandbox
		}
		if checkSandbox(ctx) != nil {
			fail(http.StatusServiceUnavailable, "council checkout confinement is unavailable")
			return
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
		if err := councils.CreateCouncil(r.Context(), room, req.spec); err != nil {
			fail(http.StatusInternalServerError, "cannot persist council")
			return
		}
		writeJSON(w, http.StatusCreated, room)
	}
}

func preflightCouncilCheckout(ctx context.Context, repo, ref, writer string, live bool, operatorToken string) (store.CouncilCheckout, error) {
	checkout := store.CouncilCheckout{}
	source, err := filepath.EvalSymlinks(repo)
	if err != nil || operatorToken != "" && strings.Contains(source, operatorToken) ||
		store.RedactCouncilAnswer(source) != source {
		return checkout, errors.New("council repo is unavailable")
	}
	gitDir := filepath.Join(source, ".git")
	if info, err := os.Lstat(gitDir); err != nil || !info.IsDir() {
		return checkout, errors.New("council repo must be a primary Git checkout")
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
		return store.CouncilCheckout{Source: source}, nil
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
	return store.CouncilCheckout{Source: source, Ref: ref, Commit: commit, WriterRole: writer}, nil
}

func ensureCouncilPanelWorkloads(ctx context.Context, st store.Store, b *config.Bundle) (*config.Bundle, error) {
	if b == nil {
		b = &config.Bundle{}
	}
	if b.Workloads == nil {
		b.Workloads = make(map[string]domain.Workload, len(councilPanel))
	}
	for _, role := range councilPanel {
		if _, exists := b.Workloads[role]; exists {
			continue
		}
		w := domain.Workload{
			Metadata: domain.ObjectMeta{Name: role, Zone: domain.ZoneSandbox},
			Spec: domain.WorkloadSpec{
				Class:  domain.ClassCodingAgent,
				Coding: &domain.CodingConfig{Harness: role, Sandbox: "seatbelt"},
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

func councilRuntimeChecker(getBundle func() *config.Bundle, adapters map[domain.WorkloadClass]scruntime.Adapter) func(context.Context, store.CouncilCheckout) error {
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
				wl.Spec.Coding == nil || wl.Spec.Coding.Harness != role || wl.Spec.Coding.Sandbox != "seatbelt" ||
				wl.Spec.Grants.ModelGate.Allowed {
				return errors.New("council panel workload unavailable: " + role)
			}
			if wl.Spec.Coding.RepoPath != "" && checkout.Source != "" {
				ra, errA := filepath.EvalSymlinks(checkout.Source)
				rb, errB := filepath.EvalSymlinks(wl.Spec.Coding.RepoPath)
				if checkout.Source != wl.Spec.Coding.RepoPath && (errA != nil || errB != nil || ra != rb) {
					return errors.New("council panel workload repo mismatch: " + role)
				}
			}
		}
		return nil
	}
}

func councilCommitID(id string) bool {
	if len(id) != 40 && len(id) != 64 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
