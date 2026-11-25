package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGateRedirect(t *testing.T) {
	host := httptest.NewRequest(http.MethodGet, "/", nil).Host
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			var requests []string
			http.DefaultTransport = transportFunc(func(r *http.Request) (*http.Response, error) {
				requests = append(requests, r.URL.Host)
				resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}
				if r.URL.Host == host {
					resp.StatusCode = status
					resp.Header.Set("Location", "https://redirect."+host)
				}
				return resp, nil
			})
			_, _, err := postJSON("https://"+host+"/actions/git.open_pr", "lease", nil)
			if err == nil || len(requests) != 1 || requests[0] != host {
				t.Fatalf("requests = %v, error = %v", requests, err)
			}
		})
	}
}
