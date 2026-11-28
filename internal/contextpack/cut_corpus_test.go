package contextpack

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// The cut corpus places one private key in a text so that a length cut of
// the builders falls on every byte of the key and just after it. The answer
// is known by construction: the key body is made of 'Z' bytes only, and no
// fixed text of the builders, filler, or redaction marker holds a 'Z', so any
// 'Z' in the output is a key byte.

const cutKeyBodyByte = "Z"

// footer shapes of a corpus key.
const (
	footerWhole = iota
	footerMissing
	footerCut
)

var cutFooterNames = []string{"whole footer", "no footer", "cut footer"}

// cutKeyLabels are the PEM labels of the corpus. The first one is cut at
// every offset; the rest at the offsets of cutKeyMarks.
var cutKeyLabels = []string{"PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY", "OPENSSH PRIVATE KEY", "ENCRYPTED PRIVATE KEY"}

// cutKey returns a key of the given label and footer shape, with LF or CRLF
// line ends, and the offsets of its body start, footer start, and end.
func cutKey(label string, footer int, nl string) (key string, bodyStart, footerStart int) {
	header := "-----" + "BEGIN " + label + "-----"
	var b strings.Builder
	b.WriteString(header + nl)
	bodyStart = b.Len()
	for range 2 {
		b.WriteString(strings.Repeat(cutKeyBodyByte, 16) + nl)
	}
	footerStart = b.Len()
	end := "-----" + "END " + label + "-----"
	switch footer {
	case footerWhole:
		b.WriteString(end)
	case footerCut:
		b.WriteString(end[:len(end)/2])
	}
	return b.String(), bodyStart, footerStart
}

// cutOffsets returns the key offsets a cut is placed at: every byte of the key
// and one past it when all is set, otherwise the start, inside the header,
// the body start, the middle of the body, the footer start, inside the
// footer, the end, and just after the end.
func cutOffsets(key string, bodyStart, footerStart int, all bool) []int {
	if all {
		out := make([]int, 0, len(key)+2)
		for p := 0; p <= len(key)+1; p++ {
			out = append(out, p)
		}
		return out
	}
	return []int{0, 7, bodyStart, (bodyStart + footerStart) / 2, footerStart, footerStart + 3, len(key), len(key) + 1}
}

type cutCase struct {
	name string
	key  string
	p    int
}

func cutCorpus() []cutCase {
	var out []cutCase
	for li, label := range cutKeyLabels {
		for footer, fname := range cutFooterNames {
			for _, nl := range []string{"\n", "\r\n"} {
				key, bs, fs := cutKey(label, footer, nl)
				for _, p := range cutOffsets(key, bs, fs, li == 0 && nl == "\n") {
					out = append(out, cutCase{name: fmt.Sprintf("%s/%s/%q/at%d", label, fname, nl, p), key: key, p: p})
				}
			}
		}
	}
	return out
}

// filler returns n bytes of text with line breaks and no 'Z'.
func filler(n int) string {
	const line = "filler line of plain words\n"
	s := strings.Repeat(line, n/len(line)+1)
	return s[:n]
}

func assertNoKeyBody(t *testing.T, c cutCase, out string) {
	t.Helper()
	if i := strings.Index(out, cutKeyBodyByte); i >= 0 {
		lo, hi := max(0, i-80), min(len(out), i+80)
		t.Fatalf("%s: key body byte in output near %q", c.name, out[lo:hi])
	}
}

// TestCutCorpusFailureHandoffArtifactTail cuts a captured artifact to its
// last 4096 bytes at every offset of a key.
func TestCutCorpusFailureHandoffArtifactTail(t *testing.T) {
	t.Parallel()
	const limit = 4096
	for _, c := range cutCorpus() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			// The cut keeps the last limit bytes, so it falls at prefix+p when
			// the text after it is limit-(len(key)-p) bytes long.
			// The key is shorter than limit, so the suffix is never empty; it
			// ends in a non-space byte so TrimSpace keeps its length.
			suffix := "\n" + filler(limit-(len(c.key)-c.p)-1)
			suffix = suffix[:len(suffix)-1] + "."
			text := filler(300) + c.key + suffix
			task := domain.Task{ID: "t-cut", WorkloadName: "coder", State: domain.TaskStateFailed, ArtifactsJSON: artifactsJSON(t,
				scruntime.Artifact{Name: "log", Text: text},
				scruntime.Artifact{Name: "next", Text: "SENTINEL-NEXT-ARTIFACT"},
			)}
			got := BuildFailureHandoff(task, nil, []domain.TaskMessage{{Role: "agent", Content: "SENTINEL-MESSAGE"}}, "boom")
			assertNoKeyBody(t, c, got)
			for _, want := range []string{"SENTINEL-NEXT-ARTIFACT", "SENTINEL-MESSAGE", "verify with tests before completing.\n"} {
				if !strings.Contains(got, want) {
					t.Fatalf("%s: handoff lost %q:\n%s", c.name, want, got)
				}
			}
		})
	}
}

// TestCutCorpusFailureHandoffMessageHead cuts a message to its first 1024
// bytes at every offset of a key.
func TestCutCorpusFailureHandoffMessageHead(t *testing.T) {
	t.Parallel()
	const limit = 1024
	for _, c := range cutCorpus() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			content := "pre " + filler(limit-c.p-4) + c.key + "\nafter the key"
			msgs := []domain.TaskMessage{
				{Role: "agent", Content: content},
				{Role: "agent", Content: "SENTINEL-NEXT-MESSAGE"},
			}
			task := domain.Task{ID: "t-cut", WorkloadName: "coder", State: domain.TaskStateFailed, ArtifactsJSON: artifactsJSON(t,
				scruntime.Artifact{Name: "next", Text: "SENTINEL-ARTIFACT"},
			)}
			got := BuildFailureHandoff(task, nil, msgs, "boom")
			assertNoKeyBody(t, c, got)
			for _, want := range []string{"SENTINEL-NEXT-MESSAGE", "SENTINEL-ARTIFACT", "verify with tests before completing.\n"} {
				if !strings.Contains(got, want) {
					t.Fatalf("%s: handoff lost %q:\n%s", c.name, want, got)
				}
			}
		})
	}
}

// TestFailureHandoffMessageKeyEndAt1500 is the case of issue 262: the END
// line of a key in a message sits at byte 1500, past the 1024 byte cut.
func TestFailureHandoffMessageKeyEndAt1500(t *testing.T) {
	t.Parallel()
	header := "-----" + "BEGIN RSA PRIVATE KEY-----\n"
	end := "-----" + "END RSA PRIVATE KEY-----"
	pre := "the agent pasted a key:\n"
	body := strings.Repeat(strings.Repeat(cutKeyBodyByte, 63)+"\n", (1500-len(pre)-len(header))/64+1)
	body = body[:1500-len(pre)-len(header)]
	content := pre + header + body + end + "\nthen kept talking"
	if got := strings.Index(content, end); got != 1500 {
		t.Fatalf("END line at %d, want 1500", got)
	}
	msgs := []domain.TaskMessage{{Role: "agent", Content: content}, {Role: "agent", Content: "SENTINEL-NEXT-MESSAGE"}}
	got := BuildFailureHandoff(domain.Task{ID: "t", WorkloadName: "coder", State: domain.TaskStateFailed}, nil, msgs, "boom")
	assertNoKeyBody(t, cutCase{name: "END at 1500"}, got)
	if !strings.Contains(got, "SENTINEL-NEXT-MESSAGE") || !strings.HasSuffix(got, "verify with tests before completing.\n") {
		t.Fatalf("handoff lost the text after the key:\n%s", got)
	}
}

// TestCutCorpusVerifierPromptArtifactHead cuts an author artifact to its
// first 4096 bytes at every offset of a key.
func TestCutCorpusVerifierPromptArtifactHead(t *testing.T) {
	t.Parallel()
	const limit = 4096
	for _, c := range cutCorpus() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			text := "pre " + filler(limit-c.p-4) + c.key + "\nafter the key"
			author := domain.Task{ID: "t-author", WorkloadName: "coder", ArtifactsJSON: artifactsJSON(t,
				scruntime.Artifact{Name: "diff", Text: text},
				scruntime.Artifact{Name: "next", Text: "SENTINEL-NEXT-ARTIFACT"},
			)}
			got := BuildVerifierPrompt(author, "check it")
			assertNoKeyBody(t, c, got)
			if !strings.Contains(got, "SENTINEL-NEXT-ARTIFACT") || !strings.HasSuffix(got, "VERDICT: CHANGES_REQUESTED\n") {
				t.Fatalf("%s: prompt lost the text after the key:\n%s", c.name, got)
			}
		})
	}
}

// TestCutCorpusRelicBodyHead cuts a Conclave relic body to its first 2048
// bytes at chosen offsets of a key.
func TestCutCorpusRelicBodyHead(t *testing.T) {
	ctx := context.Background()
	const limit = 2048
	st := newTestStore(t)
	if err := st.CreateConclave(ctx, domain.ConclaveSession{ID: "room", State: domain.ConclaveStateActive}); err != nil {
		t.Fatal(err)
	}
	for _, footer := range []int{footerWhole, footerMissing, footerCut} {
		key, bs, fs := cutKey("RSA PRIVATE KEY", footer, "\n")
		for _, p := range cutOffsets(key, bs, fs, false) {
			c := cutCase{name: fmt.Sprintf("%s/at%d", cutFooterNames[footer], p), key: key, p: p}
			body := "pre " + filler(limit-p-4) + key + "\nafter the key"
			if err := st.PutConclaveRelic(ctx, domain.ConclaveRelic{ConclaveID: "room", Name: "notes", AuthorRole: "coder", Body: body}); err != nil {
				t.Fatal(err)
			}
			task := domain.Task{ID: "t", InputJSON: `{"conclaveId":"room","prompt":"go"}`}
			md := Assemble(ctx, st, task, domain.Workload{}, Destination{}).Markdown
			assertNoKeyBody(t, c, md)
			if !strings.Contains(md, "</untrusted-peer-message>") {
				t.Fatalf("%s: relic block lost its closing tag:\n%s", c.name, md)
			}
		}
	}
}
