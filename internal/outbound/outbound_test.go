package outbound

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	cred := "hunter" + "2secret"
	cases := []struct {
		raw  string
		want string // "" means accepted
	}{
		{"https://gateway.example.test", ""},
		{"HTTPS://gateway.example.test:8443/v1", ""},
		{"http://127.0.0.1:3000", ""},
		{"http://127.9.9.9", ""},
		{"http://[::1]:3000/x", ""},
		{"http://localhost:11434", ""},
		{"http://LOCALHOST", ""},
		{"http://[::ffff:127.0.0.1]:80", ""},
		{"http://example.test", "plain http is refused"},
		{"http://10.0.0.1:3000", "plain http is refused"},
		{"http://0.0.0.0:3000", "plain http is refused"},
		{"http://[::]:3000", "plain http is refused"},
		{"http://localhost.example.test", "plain http is refused"},
		{"http://127.0.0.1.example.test", "plain http is refused"},
		{"http://user:" + cred + "@example.test", "plain http is refused"},
		{"http://127.0.0.1@example.test", "plain http is refused"},
		{"ftp://example.test", "only https"},
		{"ws://127.0.0.1", "only https"},
		{"file:///etc/passwd", "only https"},
		{"example.test:443", "only https"},
		{"//example.test", "no scheme"},
		{"gateway", "no scheme"},
		{"", "no scheme"},
		{"https://", "no host"},
		{"https://:443", "no host"},
		{"http://%zz", "not a valid URL"},
	}
	for _, tc := range cases {
		err := Check("--field", tc.raw)
		if tc.want == "" {
			if err != nil {
				t.Errorf("Check(%q) = %v, want accepted", tc.raw, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Check(%q) = %v, want an error containing %q", tc.raw, err, tc.want)
			continue
		}
		if !strings.HasPrefix(err.Error(), "--field: ") {
			t.Errorf("Check(%q) = %v, want the field named first", tc.raw, err)
		}
		if strings.Contains(err.Error(), cred) || strings.Contains(err.Error(), "example.test") {
			t.Errorf("Check(%q) = %v, leaks the URL", tc.raw, err)
		}
	}
}

func TestDisplay(t *testing.T) {
	cred := "hunter" + "2secret"
	cases := []struct{ raw, want string }{
		{"https://user:" + cred + "@gw.example.test/v1?key=" + cred + "#" + cred, "https://gw.example.test/v1"},
		{"http://127.0.0.1:3000", "http://127.0.0.1:3000"},
		{"http://%zz" + cred, "(unparsable URL)"},
		{cred, "(unparsable URL)"},
	}
	for _, tc := range cases {
		if got := Display(tc.raw); got != tc.want {
			t.Errorf("Display(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestCheckGitRemote(t *testing.T) {
	cred := "hunter" + "2secret"
	cases := []struct {
		raw  string
		want string // "" means accepted
	}{
		{"https://git.example.test/org/manifests.git", ""},
		{"ssh://git@git.example.test:2222/org/manifests.git", ""},
		{"git@git.example.test:org/manifests.git", ""},
		{"/srv/manifests", ""},
		{"file:///srv/manifests", ""},
		{"http://127.0.0.1:8080/manifests.git", ""},
		{"http://git.example.test/org/manifests.git", "plain http is refused"},
		{"http://user:" + cred + "@git.example.test/m.git", "plain http is refused"},
		{"git://git.example.test/org/manifests.git", "only https"},
		{"git://127.0.0.1/manifests.git", "only https"},
		{"ftp://git.example.test/m.git", "only https"},
		{"ext::sh -c touch% /tmp/pwned", "transport"},
		{"fd::3", "transport"},
		{"", "empty"},
	}
	for _, tc := range cases {
		err := CheckGitRemote("--field", tc.raw)
		if tc.want == "" {
			if err != nil {
				t.Errorf("CheckGitRemote(%q) = %v, want accepted", tc.raw, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("CheckGitRemote(%q) = %v, want an error containing %q", tc.raw, err, tc.want)
			continue
		}
		if !strings.HasPrefix(err.Error(), "--field: ") || strings.Contains(err.Error(), cred) || strings.Contains(err.Error(), "example.test") {
			t.Errorf("CheckGitRemote(%q) = %v, want the field named and no URL", tc.raw, err)
		}
	}
}

func TestDisplayGitRemote(t *testing.T) {
	cred := "hunter" + "2secret"
	cases := []struct{ raw, want string }{
		{"https://x-access-token:" + cred + "@git.example.test/org/m.git", "https://git.example.test/org/m.git"},
		{"git@git.example.test:org/m.git", "git@git.example.test:org/m.git"},
		{"/srv/manifests", "/srv/manifests"},
	}
	for _, tc := range cases {
		if got := DisplayGitRemote(tc.raw); got != tc.want {
			t.Errorf("DisplayGitRemote(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestClientRefusesRedirectToPlainHTTP(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	cases := []struct {
		location string
		want     string // "" means followed
	}{
		{target.URL + "/ok", ""},
		{"http://example.test/steal", "--field redirect: plain http is refused"},
		{"ftp://example.test/steal", "--field redirect: only https"},
	}
	for _, tc := range cases {
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, tc.location, http.StatusFound)
		}))
		var baseCalls int
		base := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { baseCalls++; return nil }}
		resp, err := Client(base, "--field").Get(redirector.URL)
		redirector.Close()
		if resp != nil {
			resp.Body.Close()
		}
		if tc.want == "" {
			if err != nil || resp.StatusCode != http.StatusNoContent || baseCalls != 1 {
				t.Errorf("redirect to %q: err=%v calls=%d, want followed through the base policy", tc.location, err, baseCalls)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) || baseCalls != 0 {
			t.Errorf("redirect to %q: err=%v calls=%d, want %q before the base policy", tc.location, err, baseCalls, tc.want)
		}
	}
	if base := (&http.Client{}); Client(base, "x") == base || base.CheckRedirect != nil {
		t.Fatal("Client changed or returned the injected client")
	}
}
