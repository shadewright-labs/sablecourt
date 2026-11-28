package mcpgate

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/secret"
)

// TestGitHubClientRefusesPlainHTTPAPIURL: every GitHub API call sends
// github_token, so an API base that is plain http off the machine is refused
// before the token is read or a request is made.
func TestGitHubClientRefusesPlainHTTPAPIURL(t *testing.T) {
	calls := []struct {
		name string
		call func(gh *GitHubPROpener) error
	}{
		{"open", func(gh *GitHubPROpener) error {
			_, err := gh.OpenPR(context.Background(), OpenPRInput{Owner: "acme", Repo: "app", Head: "h", Base: "main", Title: "t"})
			return err
		}},
		{"find", func(gh *GitHubPROpener) error {
			_, err := gh.FindOpenPR(context.Background(), OpenPRInput{Owner: "acme", Repo: "app", Head: "h", Base: "main"})
			return err
		}},
		{"checks", func(gh *GitHubPROpener) error {
			_, err := gh.CheckPR(context.Background(), PRChecksInput{Owner: "acme", Repo: "app", PRNumber: 1})
			return err
		}},
		{"merge", func(gh *GitHubPROpener) error {
			_, err := gh.MergePR(context.Background(), MergePRRequest{Owner: "acme", Repo: "app", PRNumber: 1, CommitSHA: strings.Repeat("a", 40)})
			return err
		}},
	}
	cases := []struct{ api, want string }{
		{"http://api.example.test", "GitHub API URL: plain http is refused"},
		{"http://u:hunter2secret@api.example.test", "GitHub API URL: plain http is refused"},
		{"ftp://api.example.test", "GitHub API URL: only https"},
	}
	for _, c := range calls {
		for _, tc := range cases {
			var sent int
			lookups := &countingStore{SecretStore: secret.NewMemoryStore(map[string]string{"github_token": "ghtoken-fixture-value"})}
			gh := &GitHubPROpener{Secrets: lookups, APIURL: tc.api, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				sent++
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
			})}}
			err := c.call(gh)
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) || strings.Contains(err.Error(), "example.test") || strings.Contains(err.Error(), "hunter2") {
				t.Errorf("%s with %q = %v, want %q and no URL", c.name, tc.api, err, tc.want)
			}
			if sent != 0 || lookups.gets != 0 {
				t.Errorf("%s with %q sent %d requests and read the token %d times, want none", c.name, tc.api, sent, lookups.gets)
			}
		}
	}
}
