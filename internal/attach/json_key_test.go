package attach

import (
	"strings"
	"testing"
)

// TestScryRedactsJSONQuotedSecretKeys pins the Scry redactor on secrets
// under JSON-quoted keys, as a Docker log line carries them, whole and split
// at every byte (issue 261, row 12 of issue 107).
func TestScryRedactsJSONQuotedSecretKeys(t *testing.T) {
	value := "hunter2" + "hunter2"
	lines := []string{
		`{"password":"` + value + `"}` + "\n",
		`{"level":"info","api_key": "` + value + `","msg":"ok"}` + "\n",
		`{"db_password" : "` + value + `"}`,
		`log {"Secret":"` + value + `"} end` + "\n",
		`{"access_token":"` + value + `"}` + "\r\n",
		`{"Authorization": "Bearer ` + value + `"}` + "\n",
	}
	for _, line := range lines {
		for cut := 0; cut <= len(line); cut++ {
			hub := NewHub()
			hub.Register("task-json", nil)
			_, ch, _, unsub, err := hub.Subscribe("task-json")
			if err != nil {
				t.Fatal(err)
			}
			hub.WriteOutput("task-json", []byte(line[:cut]))
			hub.WriteOutput("task-json", []byte(line[cut:]))
			hub.Close("task-json")
			var got strings.Builder
			for chunk := range ch {
				got.WriteString(chunk)
			}
			unsub()
			if strings.Contains(got.String(), "hunter2") {
				t.Fatalf("line %q cut at %d streamed %q", line, cut, got.String())
			}
		}
	}
}
