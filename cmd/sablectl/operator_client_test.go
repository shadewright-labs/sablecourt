package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type operatorTestTransport func(*http.Request) (*http.Response, error)

func (f operatorTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestDaemonClientOperatorCredential(t *testing.T) {
	loopback := "http://" + net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "7432")
	redirect := "https://" + net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "7433") + "/api/v1/tasks"
	upperLoopback := "http://" + net.JoinHostPort(strings.ToUpper("localhost"), "7432")
	lowerLoopback := strings.ToLower(upperLoopback)
	remote := net.IPv4zero.String()
	credential := "op_" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, baseURL, explicit, redirect string
		mode                              os.FileMode
		want                              []string
	}{
		{"default file", loopback, "", "", 0o600, []string{"Bearer " + credential}},
		{"mixed-case loopback", upperLoopback, "", "", 0o600, []string{"Bearer " + credential}},
		{"redirect to mixed-case loopback", lowerLoopback, "", upperLoopback + "/api/v1/tasks", 0o600, []string{"Bearer " + credential, ""}},
		{"unsafe file", loopback, "", "", 0o644, nil},
		{"off-host redirect", loopback, "explicit-test", redirect, 0o600, []string{"Bearer explicit-test", ""}},
		{"remote with local file", "https://" + remote, "", "", 0o600, []string{""}},
		{"remote plaintext", "http://" + remote, "explicit-test", "", 0o600, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "operator.token"), []byte(credential+"\n"), tc.mode); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SABLECOURT_STATE_DIR", dir)
			t.Setenv("SABLECOURT_OPERATOR_TOKEN", tc.explicit)
			var got []string
			old := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = old })
			http.DefaultTransport = operatorTestTransport(func(r *http.Request) (*http.Response, error) {
				auth := r.Header.Get("Authorization")
				if r.URL.Host == strings.TrimPrefix(upperLoopback, "http://") && auth != "" {
					t.Error("credential could be sent to an HTTP proxy")
				}
				got = append(got, auth)
				resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}
				if tc.redirect != "" && len(got) == 1 {
					resp.StatusCode = http.StatusFound
					resp.Header.Set("Location", tc.redirect)
				}
				return resp, nil
			})
			client, base := newDaemonClient(time.Second, tc.baseURL, "")
			res, err := client.Get(base + "/api/v1/tasks")
			if res != nil {
				res.Body.Close()
			}
			if (err != nil) != (tc.want == nil) || strings.Join(got, "|") != strings.Join(tc.want, "|") || len(got) != len(tc.want) {
				t.Errorf("sent %d requests with expected credentials = %t, request error = %v", len(got),
					strings.Join(got, "|") == strings.Join(tc.want, "|"), err)
			}
		})
	}
}

func TestDaemonClientWithCustomTransport(t *testing.T) {
	loopback := "http://" + net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "7432")
	credential := "op_" + strings.Repeat("b", 64)
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "operator.token"), []byte(credential+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SABLECOURT_STATE_DIR", dir)
	t.Setenv("SABLECOURT_OPERATOR_TOKEN", "")

	var gotAuth string
	customRT := operatorTestTransport(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("[]"))}, nil
	})
	client, base := newDaemonClientWithTransport(time.Second, loopback, "", customRT)
	res, err := client.Get(base + "/api/v1/tasks")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_ = res.Body.Close()
	if gotAuth != "Bearer "+credential {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer "+credential)
	}
}

func TestTokenFlagIsRejected(t *testing.T) {
	secret := "op_" + strings.Repeat("c", 64)
	for _, args := range [][]string{
		{"--token", secret, "validate", "."},
		{"--token=" + secret, "validate", "."},
	} {
		root := newRootCmd()
		var out strings.Builder
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Fatalf("%v: want unknown flag error, got %v", args[0], err)
		}
	}
}
