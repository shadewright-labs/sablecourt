package gatehttp

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNewClientRefusesRedirects(t *testing.T) {
	var hosts []string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hosts = append(hosts, r.URL.Host)
		return &http.Response{
			StatusCode: http.StatusTemporaryRedirect,
			Header:     http.Header{"Location": []string{"https://attacker.example/steal"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    r,
		}, nil
	})
	jar, _ := cookiejar.New(nil)
	base := &http.Client{Transport: rt, Jar: jar, Timeout: 7 * time.Second}

	cli := NewClient(base, time.Minute)
	if cli == base || cli.Transport == nil || cli.Jar != jar || cli.Timeout != 7*time.Second {
		t.Fatalf("client = %+v; want a copy that keeps the transport, jar and timeout", cli)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.provider.example/v1", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer secret")
	_, err := cli.Do(req)
	if err == nil || !strings.Contains(err.Error(), "gate request redirected") {
		t.Fatalf("Do = %v, want a redirect refusal", err)
	}
	if len(hosts) != 1 || hosts[0] != "api.provider.example" {
		t.Fatalf("requests went to %v; the redirect target must never be contacted", hosts)
	}
	if base.CheckRedirect != nil {
		t.Fatal("NewClient changed the injected client")
	}

	def := NewClient(nil, 3*time.Second)
	if def.Timeout != 3*time.Second || def.CheckRedirect == nil {
		t.Fatalf("default client = %+v", def)
	}
	if err := def.CheckRedirect(nil, nil); err == nil || !strings.Contains(err.Error(), "redirected") {
		t.Fatalf("CheckRedirect = %v", err)
	}
}
