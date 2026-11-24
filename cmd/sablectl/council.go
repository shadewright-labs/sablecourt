package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const (
	defaultCouncilRounds    = 2
	maxCouncilQuestionBytes = 64 << 10
	defaultCouncilPollEvery = 500 * time.Millisecond
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

type councilDetailResponse struct {
	Conclave domain.ConclaveSession `json:"conclave"`
	Messages []councilMessageView   `json:"messages"`
	Relics   []domain.ConclaveRelic `json:"relics"`
}

func buildCouncilCmd(httpClient func(time.Duration) (*http.Client, string), jsonOut *bool) *cobra.Command {
	var (
		repoPath   string
		refName    string
		liveMode   bool
		rounds     int
		withRaw    string
		sequential bool
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
				limited := io.LimitReader(cmd.InOrStdin(), maxCouncilQuestionBytes+1)
				line, err := bufio.NewReader(limited).ReadString('\n')
				if err != nil && !errors.Is(err, io.EOF) {
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
				JSONOut:    *jsonOut,
				PollEvery:  defaultCouncilPollEvery,
			})
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", ".", "Repository directory path")
	cmd.Flags().StringVar(&refName, "ref", "", "Pin a read-only checkout to a Git ref (mutually exclusive with --live)")
	cmd.Flags().BoolVar(&liveMode, "live", false, "Use a read-only view of the live working tree (mutually exclusive with --ref)")
	cmd.Flags().IntVar(&rounds, "rounds", defaultCouncilRounds, "Maximum council rounds")
	cmd.Flags().StringVar(&withRaw, "with", strings.Join(defaultCouncilHarnesses, ","), "Comma-separated participant harnesses (claude,codex,agy)")
	cmd.Flags().BoolVar(&sequential, "sequential", false, "Run participants sequentially within each round")
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
	JSONOut    bool
	PollEvery  time.Duration
}

func runCouncilSession(ctx context.Context, httpClient func(time.Duration) (*http.Client, string), out io.Writer, params councilRunParams) error {
	cli, base := httpClient(15 * time.Second)
	payload := buildCouncilOpenPayload(params.Question, params.Repo, params.Ref, params.Live, params.Rounds, params.Harnesses, params.Sequential)

	raw, err := fetchPOSTContext(ctx, cli, base+"/api/v1/conclaves", payload)
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

	if !params.JSONOut {
		fmt.Fprintf(out, "%s convened council %s (rounds=%d, with=%s)\n",
			stJade.Render("✓"), stTitle.Render(sess.ID), params.Rounds, strings.Join(params.Harnesses, ","))
	}

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
		lastSeenID         int64
		participantAnswers int
		lastRound          int
		roleCounts         = make(map[string]int)
		printedMessages    []councilMessageView
		latest             councilDetailResponse
	)

	syncState := func(fetchCtx context.Context) (bool, error) {
		detail, err := fetchCouncilDetail(fetchCtx, cli, base, sess.ID)
		if err != nil {
			return false, err
		}
		latest = detail
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
				participantAnswers++
				round := m.Round
				if round <= 0 {
					roleCounts[m.FromRole]++
					round = roleCounts[m.FromRole]
				}
				m.Round = round
				printedMessages = append(printedMessages, m)
				if !params.JSONOut {
					if round != lastRound {
						fmt.Fprintf(out, "\n%s\n", stTitle.Render(fmt.Sprintf("--- Round %d ---", round)))
						lastRound = round
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
					return syncErr
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
					return syncErr
				}
			} else if syncErr == nil {
				consecutiveErrs = 0
			}
		}
	}
	cancelStream()

	// Final fetch with a bounded fresh context to capture any trailing digest Relic.
	finalCtx, cancelFinal := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFinal()
	_, _ = syncState(finalCtx)

	if params.JSONOut {
		if err := writeJSONOut(out, latest); err != nil {
			return err
		}
	} else {
		printCouncilDigest(out, latest.Relics, printedMessages, params.Harnesses)
	}

	if participantAnswers == 0 {
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

func buildCouncilOpenPayload(question, repo, ref string, live bool, rounds int, harnesses []string, sequential bool) map[string]any {
	payload := map[string]any{
		"template":   "council",
		"title":      singleLineSummary(question, 120),
		"decree":     question,
		"question":   question,
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
	return payload
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

	cli, resolvedBase := httpClient(10 * time.Second)
	if base != "" {
		resolvedBase = base
	}
	escapedID := url.PathEscape(conclaveID)

	var errs []error
	if _, err := fetchPOSTContext(stopCtx, cli, resolvedBase+"/api/v1/conclaves/"+escapedID+"/close", map[string]string{
		"state": string(domain.ConclaveStateCancelled),
	}); err != nil {
		errs = append(errs, fmt.Errorf("close council: %w", err))
	}

	rawTasks, err := fetchGETContext(stopCtx, cli, resolvedBase+"/api/v1/tasks")
	if err != nil {
		errs = append(errs, fmt.Errorf("list tasks: %w", err))
		return errors.Join(errs...)
	}
	var tasks []domain.Task
	if err := json.Unmarshal(rawTasks, &tasks); err != nil {
		errs = append(errs, fmt.Errorf("decode tasks: %w", err))
		return errors.Join(errs...)
	}
	for _, task := range tasks {
		if !taskBelongsToConclave(task, conclaveID) || isTerminalTaskState(task.State) {
			continue
		}
		if _, err := fetchPOSTContext(stopCtx, cli, resolvedBase+"/api/v1/tasks/"+url.PathEscape(task.ID)+"/cancel", map[string]string{}); err != nil {
			errs = append(errs, fmt.Errorf("cancel task %s: %w", task.ID, err))
		}
	}
	return errors.Join(errs...)
}

func taskBelongsToConclave(task domain.Task, conclaveID string) bool {
	if a, ok := store.TaskConclaveAssignment(task); ok && a.ConclaveID == conclaveID {
		return true
	}
	var meta struct {
		ConclaveID string `json:"conclaveId"`
	}
	if err := json.Unmarshal([]byte(task.InputJSON), &meta); err == nil && meta.ConclaveID == conclaveID {
		return true
	}
	return strings.HasPrefix(task.IdempotencyKey, "conclave-"+conclaveID+"-")
}

func isTerminalTaskState(state domain.TaskState) bool {
	return state == domain.TaskStateCompleted || state == domain.TaskStateFailed || state == domain.TaskStateCancelled
}

func printCouncilDigest(out io.Writer, relics []domain.ConclaveRelic, msgs []councilMessageView, expectedRoles []string) {
	if digest, ok := findDigestRelic(relics); ok {
		fmt.Fprintf(out, "\n%s\n%s\n", stTitle.Render("=== Digest ==="), strings.TrimSpace(digest.Body))
		return
	}
	if len(msgs) == 0 {
		return
	}
	fmt.Fprintf(out, "\n%s\n", stTitle.Render("=== Digest ==="))
	byRound := make(map[int][]councilMessageView)
	var rounds []int
	for _, m := range msgs {
		r := m.Round
		if r <= 0 {
			r = 1
		}
		if _, ok := byRound[r]; !ok {
			rounds = append(rounds, r)
		}
		byRound[r] = append(byRound[r], m)
	}
	sort.Ints(rounds)
	for _, r := range rounds {
		fmt.Fprintf(out, "Round %d:\n", r)
		seen := make(map[string]bool)
		for _, m := range byRound[r] {
			seen[m.FromRole] = true
			fmt.Fprintf(out, "- @%s: %s\n", m.FromRole, singleLineSummary(m.Body, 240))
		}
		for _, role := range expectedRoles {
			if !seen[role] {
				fmt.Fprintf(out, "- @%s: [missing turn]\n", role)
			}
		}
	}
}

func findDigestRelic(relics []domain.ConclaveRelic) (domain.ConclaveRelic, bool) {
	for _, target := range []string{"digest", "digest.md", "council-digest", "council-digest.md"} {
		for _, r := range relics {
			if strings.EqualFold(strings.TrimSpace(r.Name), target) && strings.TrimSpace(r.Body) != "" {
				return r, true
			}
		}
	}
	return domain.ConclaveRelic{}, false
}

func validateCouncilQuestion(raw string) (string, error) {
	q := strings.TrimSpace(raw)
	if q == "" {
		return "", errors.New("question is required")
	}
	if len(q) > maxCouncilQuestionBytes {
		return "", errors.New("question exceeds 64 KiB")
	}
	if strings.ContainsRune(q, '\x00') {
		return "", errors.New("question contains invalid NUL byte")
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
	if len(parts) == 0 {
		return nil, errors.New("at least one harness is required")
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
