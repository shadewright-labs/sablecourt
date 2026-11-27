package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/shadewright-labs/sablecourt/internal/contextpack"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/waitart"
)

const (
	defaultCouncilRounds         = 2
	maxCouncilQuestionBytes      = 30 << 10
	maxCouncilEncodedPacketBytes = contextpack.MaxCouncilOpeningEncodedPacketBytes
	defaultCouncilPollEvery      = 500 * time.Millisecond
)

var defaultCouncilHarnesses = []string{"claude", "codex", "agy"}

type councilMessageView struct {
	ID         int64     `json:"id"`
	ConclaveID string    `json:"conclaveId"`
	FromRole   string    `json:"fromRole"`
	ToRole     string    `json:"toRole"`
	Kind       string    `json:"kind"`
	Body       string    `json:"body"`
	TaskID     string    `json:"taskId"`
	Round      int       `json:"round,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

type councilTurnView struct {
	Round  int    `json:"round"`
	Role   string `json:"role"`
	Status string `json:"status,omitempty"`
	State  string `json:"state,omitempty"`
}

type councilDetailResponse struct {
	Conclave domain.ConclaveSession `json:"conclave"`
	Messages []councilMessageView   `json:"messages"`
	Relics   []domain.ConclaveRelic `json:"relics"`
	Turns    []councilTurnView      `json:"turns,omitempty"`
}

func buildCouncilCmd(httpClient func(time.Duration) (*http.Client, string), jsonOut *bool) *cobra.Command {
	var (
		repoPath   string
		refName    string
		liveMode   bool
		rounds     int
		withRaw    string
		sequential bool
		writer     councilWriterFlag
	)

	cmd := &cobra.Command{
		Use:   "council [question]",
		Short: "Convene a bounded multi-agent council, stream round answers, and print the digest",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var question string
			if len(args) == 1 {
				question = args[0]
			} else {
				fmt.Fprint(cmd.ErrOrStderr(), "Council question: ")
				line, err := readLineContext(cmd.Context(), cmd.InOrStdin(), maxCouncilQuestionBytes)
				if err != nil {
					return err
				}
				question = line
			}

			question, err := validateCouncilQuestion(question)
			if err != nil {
				return err
			}

			if cmd.Flags().Changed("repo") && strings.TrimSpace(repoPath) == "" {
				return errors.New("--repo cannot be empty")
			}
			absRepo, err := validateRepoDir(repoPath)
			if err != nil {
				return err
			}

			ref, err := validateCheckoutMode(cmd.Flags().Changed("ref"), refName, liveMode)
			if err != nil {
				return err
			}

			if rounds <= 0 {
				return errors.New("--rounds must be a positive integer")
			}

			harnesses, err := parseCouncilHarnesses(withRaw)
			if err != nil {
				return err
			}
			if err := validateCouncilWriter(writer, harnesses, liveMode); err != nil {
				return err
			}
			for _, h := range harnesses {
				if _, err := exec.LookPath(h); err != nil {
					return fmt.Errorf("required harness %q not found on PATH: %w", h, err)
				}
			}

			return runCouncilSession(cmd.Context(), httpClient, cmd.OutOrStdout(), councilRunParams{
				Question:   question,
				Repo:       absRepo,
				Ref:        ref,
				Live:       liveMode,
				Rounds:     rounds,
				Harnesses:  harnesses,
				Sequential: sequential,
				Writer:     strings.TrimSpace(writer.value),
				JSONOut:    *jsonOut,
				PollEvery:  defaultCouncilPollEvery,
				ErrOut:     cmd.ErrOrStderr(),
				Art:        artConfig(cmd),
			})
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", ".", "Repository directory path")
	cmd.Flags().StringVar(&refName, "ref", "", "Pin a read-only checkout to a Git ref (mutually exclusive with --live)")
	cmd.Flags().BoolVar(&liveMode, "live", false, "Use a read-only view of the live working tree (mutually exclusive with --ref)")
	cmd.Flags().IntVar(&rounds, "rounds", defaultCouncilRounds, "Maximum council rounds")
	cmd.Flags().StringVar(&withRaw, "with", strings.Join(defaultCouncilHarnesses, ","), "Comma-separated participant harnesses (claude,codex,agy)")
	cmd.Flags().BoolVar(&sequential, "sequential", false, "Run participants sequentially within each round")
	cmd.Flags().Var(&writer, "writer", "Give one panel seat (claude, codex or agy) a writable worktree; without --ref it pins HEAD (excludes --live)")
	return cmd
}

type councilRunParams struct {
	Question   string
	Repo       string
	Ref        string
	Live       bool
	Rounds     int
	Harnesses  []string
	Sequential bool
	Writer     string
	JSONOut    bool
	PollEvery  time.Duration
	// ErrOut is where the wait animation is drawn, when Art allows it.
	ErrOut io.Writer
	Art    waitart.Config
}

func runCouncilSession(ctx context.Context, httpClient func(time.Duration) (*http.Client, string), out io.Writer, params councilRunParams) error {
	cli, base := httpClient(15 * time.Second)
	payload := buildCouncilOpenPayload(params.Question, params.Repo, params.Ref, params.Live, params.Rounds, params.Harnesses, params.Sequential, params.Writer)

	raw, err := postCouncilOpen(ctx, cli, base, payload)
	if err != nil {
		return err
	}
	var sess domain.ConclaveSession
	if err := json.Unmarshal(raw, &sess); err != nil {
		return fmt.Errorf("decode opened council: %w", err)
	}
	if strings.TrimSpace(sess.ID) == "" {
		return errors.New("daemon returned empty conclave ID")
	}

	lineWritten := true
	if !params.JSONOut {
		lineWritten = writeWhole(out, fmt.Sprintf("%s convened council %s (rounds=%d, with=%s)\n",
			stJade.Render("✓"), stTitle.Render(sess.ID), params.Rounds, strings.Join(params.Harnesses, ",")))
	}

	// The animation plays on stderr while the rounds run; council output goes
	// through it so a frame is cleared before each line and drawn below it.
	var art *waitart.Player
	if lineWritten {
		art = params.Art.Start(ctx, params.ErrOut, waitart.ForKind(waitart.Council), councilStatus(params.Rounds, councilDetailResponse{}))
	}
	defer art.Stop()
	out = art.Wrap(out)

	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	notifyCh := make(chan struct{}, 1)
	go watchCouncilStream(streamCtx, httpClient, base, sess.ID, notifyCh)

	pollEvery := params.PollEvery
	if pollEvery <= 0 {
		pollEvery = defaultCouncilPollEvery
	}
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()

	var (
		lastSeenID int64
		lastRound  int
		latest     councilDetailResponse
	)

	syncState := func(fetchCtx context.Context) (bool, error) {
		detail, err := fetchCouncilDetail(fetchCtx, cli, base, sess.ID)
		if err != nil {
			return false, err
		}
		latest = detail
		art.SetStatus(councilStatus(params.Rounds, detail))
		for _, m := range detail.Messages {
			if m.ID <= lastSeenID {
				continue
			}
			lastSeenID = m.ID
			switch m.FromRole {
			case "operator":
				continue
			case "system":
				if !params.JSONOut && strings.TrimSpace(m.Body) != "" {
					fmt.Fprintf(out, "%s %s\n", stAmber.Render("!"), strings.TrimSpace(m.Body))
				}
			default:
				if !params.JSONOut {
					if m.Round > 0 && m.Round != lastRound {
						fmt.Fprintf(out, "\n%s\n", stTitle.Render(fmt.Sprintf("--- Round %d ---", m.Round)))
						lastRound = m.Round
					}
					fmt.Fprintf(out, "%s %s\n%s\n\n",
						stJade.Render("@"+m.FromRole),
						stMuted.Render(fmt.Sprintf("(%s, #%d)", m.Kind, m.ID)),
						strings.TrimSpace(m.Body))
				}
			}
		}
		switch detail.Conclave.State {
		case domain.ConclaveStateCompleted, domain.ConclaveStateCancelled, domain.ConclaveStateInputRequired:
			return true, nil
		default:
			return false, nil
		}
	}

	const maxConsecutiveFetchErrs = 5
	consecutiveErrs := 0
	done, err := syncState(ctx)
	if err != nil {
		if ctx.Err() == nil {
			consecutiveErrs++
		}
	}

	for !done {
		select {
		case <-ctx.Done():
			cancelStream()
			stopErr := stopCouncilAndTasks(httpClient, base, sess.ID)
			return fmt.Errorf("council %s interrupted: %w", sess.ID, errors.Join(ctx.Err(), stopErr))
		case <-notifyCh:
			var syncErr error
			done, syncErr = syncState(ctx)
			if syncErr != nil && ctx.Err() == nil {
				consecutiveErrs++
				if consecutiveErrs >= maxConsecutiveFetchErrs {
					cancelStream()
					stopErr := stopCouncilAndTasks(httpClient, base, sess.ID)
					return fmt.Errorf("council %s poll failed: %w", sess.ID, errors.Join(syncErr, stopErr))
				}
			} else if syncErr == nil {
				consecutiveErrs = 0
			}
		case <-ticker.C:
			var syncErr error
			done, syncErr = syncState(ctx)
			if syncErr != nil && ctx.Err() == nil {
				consecutiveErrs++
				if consecutiveErrs >= maxConsecutiveFetchErrs {
					cancelStream()
					stopErr := stopCouncilAndTasks(httpClient, base, sess.ID)
					return fmt.Errorf("council %s poll failed: %w", sess.ID, errors.Join(syncErr, stopErr))
				}
			} else if syncErr == nil {
				consecutiveErrs = 0
			}
		}
	}
	cancelStream()
	art.Stop()

	// Final fetch with a bounded fresh context to capture any trailing digest Relic.
	finalCtx, cancelFinal := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFinal()
	_, _ = syncState(finalCtx)

	if params.JSONOut {
		if err := writeJSONOut(out, latest); err != nil {
			return err
		}
	} else {
		printCouncilDigest(out, latest.Relics)
	}

	if countCompletedCouncilTurns(latest, params.Rounds) == 0 {
		return fmt.Errorf("council %s finished with no participant answers", sess.ID)
	}
	if latest.Conclave.State == domain.ConclaveStateCancelled {
		return fmt.Errorf("council %s was cancelled", sess.ID)
	}
	if latest.Conclave.State == domain.ConclaveStateInputRequired {
		return fmt.Errorf("council %s paused by ward (%s)", sess.ID, latest.Conclave.WardReason)
	}
	return nil
}

func postCouncilOpen(ctx context.Context, cli *http.Client, base string, payload map[string]any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/councils", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "ccl_req_"+rand.Text())
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := readBoundedHTTPResponse(resp)
	if err != nil {
		var statusErr *httpStatusError
		if errors.As(err, &statusErr) && (statusErr.StatusCode == http.StatusNotFound || statusErr.StatusCode == http.StatusMethodNotAllowed) {
			return nil, fmt.Errorf("daemon does not support council endpoint POST /api/v1/councils (HTTP %d): upgrade sablecourtd", statusErr.StatusCode)
		}
		return nil, err
	}
	return raw, nil
}

func countCompletedCouncilTurns(detail councilDetailResponse, maxRounds int) int {
	if len(detail.Turns) > 0 {
		n := 0
		for _, t := range detail.Turns {
			if t.Round <= 0 || (maxRounds > 0 && t.Round > maxRounds) {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(t.Status), "completed") || strings.EqualFold(strings.TrimSpace(t.State), "completed") {
				n++
			}
		}
		return n
	}
	n := 0
	for _, m := range detail.Messages {
		if m.FromRole == "operator" || m.FromRole == "system" {
			continue
		}
		if m.Round <= 0 || (maxRounds > 0 && m.Round > maxRounds) {
			continue
		}
		if isCouncilAnswerKind(m.Kind) {
			n++
		}
	}
	return n
}

func isCouncilAnswerKind(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "proposal", "review", "decision", "answer":
		return true
	default:
		return false
	}
}

type readDeadlineSetter interface {
	SetReadDeadline(time.Time) error
}

func readLineContext(ctx context.Context, r io.Reader, maxBytes int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	type result struct {
		line      string
		exhausted bool
		err       error
	}
	ch := make(chan result, 1)
	go func() {
		// Allow up to maxBytes plus a CRLF terminator plus one sentinel byte so
		// hitting the reader limit before '\n' is detected before trimming.
		lr := &io.LimitedReader{R: r, N: maxBytes + 3}
		line, err := bufio.NewReader(lr).ReadString('\n')
		ch <- result{line: line, exhausted: errors.Is(err, io.EOF) && lr.N == 0, err: err}
	}()
	select {
	case res := <-ch:
		if res.exhausted {
			return "", fmt.Errorf("question exceeds %d KiB", maxBytes>>10)
		}
		if res.err != nil && !errors.Is(res.err, io.EOF) {
			return "", res.err
		}
		return res.line, nil
	case <-ctx.Done():
		if ds, ok := r.(readDeadlineSetter); ok {
			_ = ds.SetReadDeadline(time.Now())
		}
		return "", ctx.Err()
	}
}

func buildCouncilOpenPayload(question, repo, ref string, live bool, rounds int, harnesses []string, sequential bool, writer string) map[string]any {
	redactedQuestion := store.RedactCouncilAnswer(question)
	payload := map[string]any{
		"title":      singleLineSummary(redactedQuestion, 120),
		"decree":     redactedQuestion,
		"question":   redactedQuestion,
		"rounds":     rounds,
		"with":       harnesses,
		"sequential": sequential,
	}
	if repo != "" {
		payload["repo"] = repo
	}
	if ref != "" {
		payload["ref"] = ref
	}
	if live {
		payload["live"] = true
	}
	if writer != "" {
		payload["writer"] = writer
	}
	return payload
}

// councilWriterFlag holds --writer. It refuses a second value instead of
// letting the last one win silently.
type councilWriterFlag struct {
	value string
	set   bool
}

func (f *councilWriterFlag) String() string { return f.value }

func (f *councilWriterFlag) Type() string { return "seat" }

func (f *councilWriterFlag) Set(v string) error {
	if f.set {
		return errors.New("--writer can be given only once")
	}
	f.value = v
	f.set = true
	return nil
}

// validateCouncilWriter checks --writer against the panel the command sends,
// before any request.
func validateCouncilWriter(writer councilWriterFlag, harnesses []string, live bool) error {
	if !writer.set {
		return nil
	}
	seat := strings.TrimSpace(writer.value)
	if seat == "" {
		return errors.New("--writer cannot be empty")
	}
	if !slices.Contains(harnesses, seat) {
		return fmt.Errorf("--writer %q is not a seat on the panel (%s)", writer.value, strings.Join(harnesses, ", "))
	}
	if live {
		return errors.New("--writer and --live are mutually exclusive")
	}
	return nil
}

func watchCouncilStream(ctx context.Context, httpClient func(time.Duration) (*http.Client, string), base, conclaveID string, notifyCh chan<- struct{}) {
	streamCli, _ := httpClient(0)
	escapedID := url.PathEscape(conclaveID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/conclaves/"+escapedID+"/stream", nil)
	if err != nil {
		return
	}
	resp, err := streamCli.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			select {
			case notifyCh <- struct{}{}:
			default:
			}
		}
	}
}

func fetchCouncilDetail(ctx context.Context, cli *http.Client, base, conclaveID string) (councilDetailResponse, error) {
	escapedID := url.PathEscape(conclaveID)
	body, err := fetchGETContext(ctx, cli, base+"/api/v1/conclaves/"+escapedID)
	if err != nil {
		return councilDetailResponse{}, err
	}
	var res councilDetailResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return councilDetailResponse{}, err
	}
	return res, nil
}

func stopCouncilAndTasks(httpClient func(time.Duration) (*http.Client, string), base, conclaveID string) error {
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := closeConclaveAndStopTasks(stopCtx, httpClient, base, conclaveID, domain.ConclaveStateCancelled)
	return err
}

func closeConclaveAndStopTasks(ctx context.Context, httpClient func(time.Duration) (*http.Client, string), base, conclaveID string, state domain.ConclaveState) ([]byte, error) {
	cli, resolvedBase := httpClient(5 * time.Second)
	if base != "" {
		resolvedBase = base
	}
	escapedID := url.PathEscape(conclaveID)

	var errs []error
	closeBody, err := fetchPOSTContext(ctx, cli, resolvedBase+"/api/v1/conclaves/"+escapedID+"/close", map[string]string{
		"state": string(state),
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("close council: %w", err))
	}

	tasks, err := listTasksContext(ctx, cli, resolvedBase)
	if err != nil {
		errs = append(errs, fmt.Errorf("list tasks: %w", err))
		return closeBody, errors.Join(errs...)
	}

	var activeOwned []domain.Task
	for _, task := range tasks {
		if !taskBelongsToConclave(task, conclaveID) || isTerminalTaskState(task.State) {
			continue
		}
		activeOwned = append(activeOwned, task)
	}

	for _, task := range activeOwned {
		if _, err := fetchPOSTContext(ctx, cli, resolvedBase+"/api/v1/tasks/"+url.PathEscape(task.ID)+"/cancel", map[string]string{}); err != nil {
			errs = append(errs, fmt.Errorf("cancel task %s: %w", task.ID, err))
		}
	}

	if len(activeOwned) > 0 {
		afterTasks, err := listTasksContext(ctx, cli, resolvedBase)
		if err != nil {
			errs = append(errs, fmt.Errorf("verify tasks after close: %w", err))
			return closeBody, errors.Join(errs...)
		}
		for _, task := range afterTasks {
			if taskBelongsToConclave(task, conclaveID) && !isTerminalTaskState(task.State) {
				errs = append(errs, fmt.Errorf("owned task %s is still %s after close", task.ID, task.State))
			}
		}
	}
	return closeBody, errors.Join(errs...)
}

func listTasksContext(ctx context.Context, cli *http.Client, base string) ([]domain.Task, error) {
	rawTasks, err := fetchGETContext(ctx, cli, base+"/api/v1/tasks")
	if err != nil {
		return nil, err
	}
	var tasks []domain.Task
	if err := json.Unmarshal(rawTasks, &tasks); err != nil {
		return nil, fmt.Errorf("decode tasks: %w", err)
	}
	return tasks, nil
}

func taskBelongsToConclave(task domain.Task, conclaveID string) bool {
	if conclaveID == "" {
		return false
	}
	a, ok := store.TaskConclaveAssignment(task)
	return ok && a.ConclaveID == conclaveID
}

func isTerminalTaskState(state domain.TaskState) bool {
	return state == domain.TaskStateCompleted || state == domain.TaskStateFailed || state == domain.TaskStateCancelled
}

func printCouncilDigest(out io.Writer, relics []domain.ConclaveRelic) {
	if digest, ok := findDigestRelic(relics); ok {
		fmt.Fprintf(out, "\n%s\n%s\n", stTitle.Render("=== Digest ==="), strings.TrimSpace(digest.Body))
		return
	}
	fmt.Fprintf(out, "\n%s\nno digest recorded\n", stTitle.Render("=== Digest ==="))
}

func findDigestRelic(relics []domain.ConclaveRelic) (domain.ConclaveRelic, bool) {
	for _, target := range []string{"digest", "digest.md", "council-digest", "council-digest.md"} {
		for _, r := range relics {
			if strings.TrimSpace(r.AuthorRole) != "system" {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(r.Name), target) && strings.TrimSpace(r.Body) != "" {
				return r, true
			}
		}
	}
	return domain.ConclaveRelic{}, false
}

func validateCouncilQuestion(raw string) (string, error) {
	if strings.ContainsRune(raw, '\x00') {
		return "", errors.New("question contains invalid NUL byte")
	}
	trimmedLine := raw
	if strings.HasSuffix(trimmedLine, "\n") {
		trimmedLine = strings.TrimSuffix(trimmedLine, "\n")
		trimmedLine = strings.TrimSuffix(trimmedLine, "\r")
	}
	if len(trimmedLine) > maxCouncilQuestionBytes {
		return "", errors.New("question exceeds 30 KiB")
	}
	q := strings.TrimSpace(raw)
	if q == "" {
		return "", errors.New("question is required")
	}
	redacted := store.RedactCouncilAnswer(q)
	if len(redacted) > maxCouncilQuestionBytes {
		return "", errors.New("question exceeds 30 KiB after redaction")
	}
	packet, err := contextpack.AssembleCouncil("", redacted, nil, nil)
	if err != nil {
		return "", errors.New("question cannot be packed")
	}
	encoded, err := json.Marshal(packet.Markdown)
	if err != nil || len(encoded) > maxCouncilEncodedPacketBytes {
		return "", errors.New("question exceeds launch input budget")
	}
	return q, nil
}

func validateRepoDir(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		p = "."
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve --repo: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("invalid --repo %q: directory does not exist", raw)
	}
	return abs, nil
}

func validateCheckoutMode(refSet bool, rawRef string, live bool) (string, error) {
	ref := strings.TrimSpace(rawRef)
	if refSet && ref == "" {
		return "", errors.New("--ref cannot be empty")
	}
	if ref != "" && live {
		return "", errors.New("--ref and --live are mutually exclusive")
	}
	if ref != "" {
		if strings.HasPrefix(ref, "-") || strings.Contains(ref, "..") || hasControlChars(ref) || strings.ContainsAny(ref, " \t\n\r") {
			return "", fmt.Errorf("invalid git ref %q", rawRef)
		}
	}
	return ref, nil
}

func parseCouncilHarnesses(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	return validateCouncilHarnessSlice(parts)
}

func validateCouncilHarnessSlice(parts []string) ([]string, error) {
	if len(parts) != len(defaultCouncilHarnesses) {
		return nil, errors.New("council panel requires claude, codex, and agy")
	}
	seen := make(map[string]bool, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		h := strings.TrimSpace(p)
		switch h {
		case "claude", "codex", "agy":
		default:
			if h == "" {
				return nil, errors.New("empty harness in participant list")
			}
			return nil, fmt.Errorf("unsupported harness %q (allowed: claude, codex, agy)", h)
		}
		if seen[h] {
			return nil, fmt.Errorf("duplicate harness %q", h)
		}
		seen[h] = true
		out = append(out, h)
	}
	return out, nil
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func singleLineSummary(s string, max int) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}
