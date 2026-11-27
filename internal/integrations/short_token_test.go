package integrations

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/secret"
)

// An integration token too short to redact is refused on the integration
// routes and on every other route, even when the caller sends it.
func TestShortIntegrationTokenRefused(t *testing.T) {
	const short = "short12"
	sec := secret.NewMemoryStore(map[string]string{ShadewrightTokenSecretName: short})

	h := ShadewrightHandler{secrets: sec, secretName: ShadewrightTokenSecretName}
	r := httptest.NewRequest(http.MethodPost, ShadewrightIncidentPath, nil)
	r.Header.Set("Authorization", "Bearer "+short)
	w := httptest.NewRecorder()
	if h.authenticate(w, r) || w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), short) {
		t.Fatalf("authenticate with a short token: %d %s", w.Code, w.Body.String())
	}

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r = httptest.NewRequest(http.MethodGet, "/api/v1/workloads", nil)
	r.Header.Set("Authorization", "Bearer "+short)
	w = httptest.NewRecorder()
	RejectShadewrightTokenOnOtherRoutes(next, sec).ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), short) {
		t.Fatalf("other route with a short token: %d %s", w.Code, w.Body.String())
	}
}
