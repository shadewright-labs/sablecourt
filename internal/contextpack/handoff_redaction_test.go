package contextpack

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact/redacttest"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// keyBlock builds a private key block whose END marker ends at byte end.
func keyBlock(end int) (string, string) {
	const header = "-----BEGIN RSA PRIVATE KEY-----\n"
	const footer = "\n-----END RSA PRIVATE KEY-----"
	body := strings.Repeat("Q7zx", (end-len(header)-len(footer))/4+1)[:end-len(header)-len(footer)]
	return header + body + footer, body
}

func TestFailureHandoffRedactsBeforeTruncating(t *testing.T) {
	msgKey, msgBody := keyBlock(1500)
	artKey, artBody := keyBlock(1500)
	// The artifact keeps only its last 4096 bytes, so its header is cut off
	// and only the body and END marker would remain.
	artText := artKey + "\n" + strings.Repeat("log line\n", 400)
	arts, err := json.Marshal([]scruntime.Artifact{{Name: "session-log", Text: artText}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		task domain.Task
		msgs []domain.TaskMessage
		body string
	}{
		{"message END marker past the cut", domain.Task{ID: "t1"}, []domain.TaskMessage{{Role: "agent", Content: msgKey}}, msgBody},
		{"artifact header before the cut", domain.Task{ID: "t2", ArtifactsJSON: string(arts)}, nil, artBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := BuildFailureHandoff(tc.task, nil, tc.msgs, "failed")
			if frag, ok := redacttest.Leak(out, []string{tc.body}); ok {
				t.Fatalf("key fragment %q in handoff (%d bytes)", frag, len(out))
			}
		})
	}
}
