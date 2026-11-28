package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/outbound"
)

type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// GitHubDeviceFlow implements RFC 8628 GitHub OAuth Device Flow so operators can authenticate
// GitHub Actions/PR credentials without manually generating and pasting PAT strings.
type GitHubDeviceFlow struct {
	ClientID string
	BaseURL  string
	Client   *http.Client
}

func NewGitHubDeviceFlow(clientID string) *GitHubDeviceFlow {
	return &GitHubDeviceFlow{
		ClientID: clientID,
		BaseURL:  "https://github.com",
		Client:   &http.Client{Timeout: 15 * time.Second},
	}
}

func (g *GitHubDeviceFlow) Start(ctx context.Context, scopes string) (DeviceCodeResponse, error) {
	if strings.TrimSpace(g.ClientID) == "" {
		return DeviceCodeResponse{}, errors.New("GitHub OAuth client_id is required")
	}
	if scopes == "" {
		scopes = "repo"
	}
	if err := g.checkBaseURL(); err != nil {
		return DeviceCodeResponse{}, err
	}
	form := url.Values{
		"client_id": {g.ClientID},
		"scope":     {scopes},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(g.BaseURL, "/")+"/login/device/code", strings.NewReader(form.Encode()))
	if err != nil {
		return DeviceCodeResponse{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := g.httpClient().Do(req)
	if err != nil {
		return DeviceCodeResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return DeviceCodeResponse{}, fmt.Errorf("github device/code returned HTTP %d", resp.StatusCode)
	}
	var out DeviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return DeviceCodeResponse{}, err
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		return DeviceCodeResponse{}, errors.New("incomplete device code response from GitHub")
	}
	return out, nil
}

func (g *GitHubDeviceFlow) PollToken(ctx context.Context, deviceCode string, interval time.Duration) (string, error) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if err := g.checkBaseURL(); err != nil {
		return "", err
	}
	for {
		form := url.Values{
			"client_id":   {g.ClientID},
			"device_code": {deviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(g.BaseURL, "/")+"/login/oauth/access_token", strings.NewReader(form.Encode()))
		if err != nil {
			return "", err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, err := g.httpClient().Do(req)
		if err != nil {
			return "", err
		}
		var tok struct {
			AccessToken string `json:"access_token"`
			Error       string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&tok)
		resp.Body.Close()

		if tok.AccessToken != "" {
			return tok.AccessToken, nil
		}
		switch tok.Error {
		case "authorization_pending":
			// Wait for next poll interval.
		case "slow_down":
			interval += 5 * time.Second
		case "expired_token", "access_denied":
			return "", fmt.Errorf("github device flow failed: %s", tok.Error)
		default:
			if tok.Error != "" {
				return "", fmt.Errorf("github oauth error: %s", tok.Error)
			}
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (g *GitHubDeviceFlow) httpClient() *http.Client {
	cli := g.Client
	if cli == nil {
		cli = &http.Client{Timeout: 15 * time.Second}
	}
	return outbound.Client(cli, "GitHub OAuth base URL")
}

// checkBaseURL refuses a base URL that would carry the device code and the
// returned access token in plain text off the machine.
func (g *GitHubDeviceFlow) checkBaseURL() error {
	return outbound.Check("GitHub OAuth base URL", g.BaseURL)
}
