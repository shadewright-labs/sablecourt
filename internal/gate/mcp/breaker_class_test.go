package mcpgate

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/secret"
)

// Medium 6: which upstream answers count on the breaker. A 4xx caused by the
// request (422, 404, 409) does not; 401, 403 and 429 mean the daemon's own
// token or quota fails and do; 5xx and timeouts do.
func TestBreakerCountsOnlyDependencyFailures(t *testing.T) {
	for _, tc := range []struct {
		status int
		counts bool
	}{
		{422, false}, {404, false}, {409, false}, {400, false},
		{401, true}, {403, true}, {429, true}, {500, true}, {502, true}, {503, true},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			opener := &GitHubPROpener{Secrets: secret.NewMemoryStore(map[string]string{"github_token": "t"}), APIURL: srv.URL}
			tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectAllow, opener: opener})
			for i := 0; i < 3; i++ {
				rec := tg.post("ops-agent", "git.open_pr", `{"payload":`+validOpenPR+`}`)
				if rec.Code == http.StatusOK {
					t.Fatalf("call %d: upstream %d answered 200", i+1, tc.status)
				}
			}
			rec := tg.post("ops-agent", "git.open_pr", `{"payload":`+validOpenPR+`}`)
			open := strings.Contains(rec.Body.String(), "circuit_breaker_open")
			if open != tc.counts {
				t.Fatalf("upstream %d three times: breaker open=%v, want %v (%d %s)", tc.status, open, tc.counts, rec.Code, rec.Body.String())
			}
		})
	}
}

// Medium 6: a timeout counts.
func TestBreakerCountsUpstreamTimeouts(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)
	opener := &GitHubPROpener{Secrets: secret.NewMemoryStore(map[string]string{"github_token": "t"}), APIURL: srv.URL, Timeout: 50 * time.Millisecond}
	tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectAllow, opener: opener})
	for i := 0; i < 3; i++ {
		tg.post("ops-agent", "git.open_pr", `{"payload":`+validOpenPR+`}`)
	}
	if rec := tg.post("ops-agent", "git.open_pr", `{"payload":`+validOpenPR+`}`); !strings.Contains(rec.Body.String(), "circuit_breaker_open") {
		t.Fatalf("three timeouts must open the breaker, got %d %s", rec.Code, rec.Body.String())
	}
}

// Medium 6: a checkout whose origin cannot be read is the daemon's own failing
// dependency, so it counts.
func TestBreakerCountsAnUnreadableCheckout(t *testing.T) {
	tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectAllow})
	if out, err := exec.Command("git", "-C", tg.repo, "remote", "remove", "origin").CombinedOutput(); err != nil {
		t.Fatalf("git remote remove: %v (%s)", err, out)
	}
	for i := 0; i < 3; i++ {
		if rec := tg.post("ops-agent", "git.open_pr", `{"payload":`+validOpenPR+`}`); rec.Code == http.StatusOK {
			t.Fatal("unreadable checkout answered 200")
		}
	}
	if rec := tg.post("ops-agent", "git.open_pr", `{"payload":`+validOpenPR+`}`); !strings.Contains(rec.Body.String(), "circuit_breaker_open") {
		t.Fatalf("three unreadable-checkout failures must open the breaker, got %d %s", rec.Code, rec.Body.String())
	}
}
