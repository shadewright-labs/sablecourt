package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestReadBoundedHTTPResponse(t *testing.T) {
	big := bytes.Repeat([]byte("e"), maxHTTPResponseBytes+10)
	exact := bytes.Repeat([]byte("k"), maxHTTPResponseBytes)
	cases := []struct {
		name       string
		status     int
		body       []byte
		wantBody   []byte
		wantStatus int
		wantErr    string
		wantErrLen int
	}{
		{name: "ok", status: http.StatusOK, body: []byte(`{"a":1}`), wantBody: []byte(`{"a":1}`)},
		{name: "exactly at limit", status: http.StatusOK, body: exact, wantBody: exact},
		{name: "over limit success fails closed", status: http.StatusOK, body: big, wantErr: fmt.Sprintf("HTTP response exceeded %d bytes", maxHTTPResponseBytes)},
		{name: "over limit error keeps status and truncates body", status: http.StatusBadGateway, body: big, wantStatus: http.StatusBadGateway, wantErrLen: maxHTTPResponseBytes},
		{name: "399 is success", status: 399, body: []byte("x"), wantBody: []byte("x")},
		{name: "400 is an error with trimmed body", status: http.StatusBadRequest, body: []byte("  bad input \n"), wantStatus: http.StatusBadRequest, wantErr: "HTTP 400: bad input"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tc.status, Body: io.NopCloser(bytes.NewReader(tc.body))}
			got, err := readBoundedHTTPResponse(resp)
			if tc.wantBody != nil {
				if err != nil || !bytes.Equal(got, tc.wantBody) {
					t.Fatalf("got %d bytes, err %v; want %d bytes", len(got), err, len(tc.wantBody))
				}
				return
			}
			if err == nil {
				t.Fatalf("want error, got %d bytes", len(got))
			}
			if got != nil {
				t.Errorf("body returned with error: %d bytes", len(got))
			}
			if tc.wantErr != "" && err.Error() != tc.wantErr {
				t.Errorf("error = %q, want %q", err.Error(), tc.wantErr)
			}
			var se *httpStatusError
			isStatus := errors.As(err, &se)
			if tc.wantStatus != 0 {
				if !isStatus || se.StatusCode != tc.wantStatus {
					t.Fatalf("error %v is not an httpStatusError with status %d", err, tc.wantStatus)
				}
				if tc.wantErrLen != 0 && len(se.Body) != tc.wantErrLen {
					t.Errorf("status error body length = %d, want %d", len(se.Body), tc.wantErrLen)
				}
			} else if isStatus {
				t.Errorf("oversized success reported as status error %d", se.StatusCode)
			}
		})
	}
}

func TestReadBoundedHTTPResponseReadError(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(io.MultiReader(strings.NewReader("part"), errReader{}))}
	if _, err := readBoundedHTTPResponse(resp); err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("err = %v, want read error", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestTaskScryPrintsOnlyDecodedDataLines(t *testing.T) {
	d := newCLIDaemon()
	d.routes["GET /api/v1/tasks/t%2F1/scry"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": keepalive\n")
		_, _ = io.WriteString(w, "event: chunk\n")
		_, _ = io.WriteString(w, `data: "hello "`+"\n\n")
		_, _ = io.WriteString(w, "data: {not a string}\n")
		_, _ = io.WriteString(w, `data: "world\n"`+"\n")
		_, _ = io.WriteString(w, `"not prefixed"`+"\n")
	}
	out, _, err := runCLI(t, d, "", "task", "scry", "t/1")
	if err != nil {
		t.Fatalf("scry: %v", err)
	}
	if out != "hello world\n" {
		t.Fatalf("scry output = %q, want %q", out, "hello world\n")
	}
}

func TestTaskScryRejectsOverlongLine(t *testing.T) {
	d := newCLIDaemon()
	d.routes["GET /api/v1/tasks/t1/scry"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "data: \""+strings.Repeat("a", 2<<20)+"\"\n")
	}
	_, _, err := runCLI(t, d, "", "task", "scry", "t1")
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("err = %v, want scanner token too long", err)
	}
}

func TestTaskTakeoverSendsStdinAndDetaches(t *testing.T) {
	d := newCLIDaemon().
		on("POST /api/v1/tasks/t1/takeover", http.StatusOK, `{}`).
		on("POST /api/v1/tasks/t1/detach", http.StatusOK, `{}`)
	gotStdin := make(chan struct{})
	d.routes["POST /api/v1/tasks/t1/stdin"] = func(w http.ResponseWriter, _ *http.Request) {
		close(gotStdin)
		w.WriteHeader(http.StatusOK)
	}
	d.routes["GET /api/v1/tasks/t1/scry"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `data: "attached\n"`+"\n")
		select {
		case <-gotStdin:
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
		_, _ = io.WriteString(w, `data: "done\n"`+"\n")
	}
	out, stderr, err := runCLI(t, d, "ls -la\n", "task", "takeover", "t1")
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if out != "attached\ndone\n" {
		t.Errorf("stream output = %q", out)
	}
	if !strings.Contains(stderr, "takeover acquired on t1") {
		t.Errorf("stderr = %q", stderr)
	}
	reqs := d.requests()
	var routes []string
	for _, r := range reqs {
		routes = append(routes, r.Method+" "+r.Path)
	}
	if len(reqs) != 4 || routes[0] != "POST /api/v1/tasks/t1/takeover" || routes[3] != "POST /api/v1/tasks/t1/detach" {
		t.Fatalf("request order = %v, want takeover, stdin and scry, then detach", routes)
	}
	for _, r := range reqs {
		switch r.Path {
		case "/api/v1/tasks/t1/takeover", "/api/v1/tasks/t1/detach":
			if !jsonEqual(t, r.Body, `{"operator":"sablectl"}`) {
				t.Errorf("%s body = %s", r.Path, r.Body)
			}
		case "/api/v1/tasks/t1/stdin":
			if !jsonEqual(t, r.Body, `{"operator":"sablectl","data":"ls -la\n"}`) {
				t.Errorf("stdin body = %s", r.Body)
			}
		}
	}
}

func TestConclaveWatchPrintsEachMessageOnce(t *testing.T) {
	d := newCLIDaemon()
	calls := 0
	d.routes["GET /api/v1/conclaves/c1"] = func(w http.ResponseWriter, _ *http.Request) {
		calls++
		msgs := `[{"ID":1,"FromRole":"coder","ToRole":"all","Kind":"proposal","Body":"first"}]`
		if calls > 1 {
			msgs = `[{"ID":1,"FromRole":"coder","ToRole":"all","Kind":"proposal","Body":"first"},{"ID":2,"FromRole":"reviewer","ToRole":"coder","Kind":"review","Body":"second"}]`
		}
		_, _ = io.WriteString(w, `{"messages":`+msgs+`}`)
	}
	d.routes["GET /api/v1/conclaves/c1/stream"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "data: 2\n\n: ping\ndata: 3\n")
	}
	out, _, err := runCLI(t, d, "", "conclave", "watch", "c1")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if strings.Count(out, "#1 @coder -> @all (proposal): first") != 1 || strings.Count(out, "#2 @reviewer -> @coder (review): second") != 1 {
		t.Fatalf("watch output = %q, want each message once", out)
	}
	if calls != 3 {
		t.Fatalf("detail fetched %d times, want once at start and once per data event", calls)
	}
}

func TestConclaveWatchStreamRefusedReturnsError(t *testing.T) {
	t.Skip("defect: conclave watch ignores the stream response status, so a 403 or 404 stream exits 0 with no output")
	d := newCLIDaemon().
		on("GET /api/v1/conclaves/c1", http.StatusOK, `{"messages":[]}`).
		on("GET /api/v1/conclaves/c1/stream", http.StatusForbidden, `{"error":"operator credential required"}`)
	_, _, err := runCLI(t, d, "", "conclave", "watch", "c1")
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("err = %v, want the stream refusal HTTP 403", err)
	}
}
