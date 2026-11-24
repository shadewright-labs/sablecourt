package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAuditExportStreamsPast8MiB(t *testing.T) {
	const (
		targetBytes = maxHTTPResponseBytes + (1 << 20) // 9 MiB
		padSize     = 4096
	)
	pad := strings.Repeat("a", padSize)
	lineCount := (targetBytes / padSize) + 2

	rt := &handlerRoundTripper{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/audit/export" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		for i := 1; i <= lineCount; i++ {
			_, _ = fmt.Fprintf(w, "{\"seq\":%d,\"detail\":%q}\n", i, pad)
		}
	})}

	root := newRootCmdWithTransport(rt)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"--api", "http://unix", "audit", "export"})

	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("audit export failed: %v (stderr=%s)", err, stderr.String())
	}

	raw := stdout.Bytes()
	if len(raw) <= maxHTTPResponseBytes {
		t.Fatalf("expected streamed output > %d bytes, got %d bytes", maxHTTPResponseBytes, len(raw))
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != lineCount {
		t.Fatalf("exported line count = %d, want %d", len(lines), lineCount)
	}
	var last struct {
		Seq    int    `json:"seq"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("last exported JSONL record is invalid JSON: %v", err)
	}
	if last.Seq != lineCount || len(last.Detail) != padSize {
		t.Fatalf("unexpected last record: seq=%d detailLen=%d, want seq=%d detailLen=%d",
			last.Seq, len(last.Detail), lineCount, padSize)
	}
}

func TestFetchGETContextFailsClosedAboveLimit(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		for written := 0; written <= maxHTTPResponseBytes; written += len(chunk) {
			_, _ = w.Write(chunk)
		}
	})
	cli, base := newTestClientFactory(handler)(5 * time.Second)
	_, err := fetchGETContext(context.Background(), cli, base+"/api/v1/workloads")
	if err == nil {
		t.Fatal("expected error when non-streaming response exceeds limit, got nil")
	}
}
