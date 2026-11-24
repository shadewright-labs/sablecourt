package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func studioSave(ctx context.Context, handler http.Handler, name string) string {
	form := url.Values{
		"name":            {name},
		"vassal_role":     {"reviewer"},
		"vassal_workload": {"review-worker"},
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/ui/studio/conclave", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Header().Get("Location")
}

func TestStudioRollbackPreservesIndependentSaves(t *testing.T) {
	const original = "# original"
	const replacement = "# independently saved manifest\n"
	for _, name := range []string{"replacement file", "same-size edit to original bytes", "same-size edit to appended bytes"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manifest.yaml")
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			var want string
			reload := func() error {
				if name == "replacement file" {
					want = replacement
					if err := os.Rename(path, path+".previous"); err != nil {
						t.Fatal(err)
					}
				} else {
					raw, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					at := 0
					if name == "same-size edit to appended bytes" {
						at = len(raw) - 1
					}
					raw[at] = '!'
					want = string(raw)
				}
				if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
					t.Fatal(err)
				}
				return errors.New("reload failed")
			}
			dash, err := NewDashboard(Options{ManifestsPath: path, Reload: reload})
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			dash.RegisterRoutes(mux)

			if loc := studioSave(t.Context(), mux, "review"); !strings.Contains(loc, "reload+failed") {
				t.Errorf("save did not report reload failure: %s", loc)
			}
			raw, err := os.ReadFile(path)
			if err != nil || string(raw) != want {
				t.Errorf("independent save = %q, want %q, err = %v", raw, want, err)
			}
			if name == "replacement file" {
				prior, err := os.ReadFile(path + ".previous")
				if err != nil || string(prior) != original {
					t.Errorf("previous manifest = %q, want %q, err = %v", prior, original, err)
				}
			}
		})
	}
}

func TestStudioCanceledSaveDoesNotWaitOrWrite(t *testing.T) {
	dir := t.TempDir()
	reloading := make(chan struct{})
	release := make(chan struct{})
	first := make(chan string, 1)
	second := make(chan string, 1)
	reloads := 0
	dash, err := NewDashboard(Options{ManifestsPath: dir, Reload: func() error {
		reloads++
		if reloads == 1 {
			close(reloading)
			<-release
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	dash.RegisterRoutes(mux)
	go func() { first <- studioSave(t.Context(), mux, "first-save") }()
	<-reloading

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { second <- studioSave(ctx, mux, "canceled-save") }()
	cancel()
	waiting := false
	select {
	case loc := <-second:
		if !strings.Contains(loc, "context+canceled") {
			t.Errorf("canceled save redirect = %q", loc)
		}
	case <-time.After(time.Second):
		t.Error("canceled save waited for the manifest lock")
		waiting = true
	}
	close(release)
	if loc := <-first; !strings.HasPrefix(loc, "/studio?notice=") {
		t.Errorf("first save redirect = %q", loc)
	}
	if waiting {
		<-second
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Errorf("manifest directory has %d files, want 1, err = %v", len(files), err)
	}
	if reloads != 1 {
		t.Errorf("reload called %d times, want 1", reloads)
	}
}
