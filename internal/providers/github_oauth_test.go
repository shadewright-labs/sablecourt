package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestGitHubDeviceFlowStartAndPoll(t *testing.T) {
	var polls atomic.Int32
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login/device/code":
			_, _ = w.Write([]byte(`{"device_code":"dev-123","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","expires_in":900,"interval":1}`))
		case "/login/oauth/access_token":
			if polls.Add(1) == 1 {
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"gho_real_oauth_token_123456"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	flow := &GitHubDeviceFlow{
		ClientID: "Iv1.testclient",
		BaseURL:  "https://github.test",
		Client: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, r)
				return rec.Result(), nil
			}),
		},
	}

	dc, err := flow.Start(context.Background(), "repo")
	if err != nil || dc.UserCode != "ABCD-1234" {
		t.Fatalf("Start failed: %+v err=%v", dc, err)
	}

	tok, err := flow.PollToken(context.Background(), dc.DeviceCode, 5*time.Millisecond)
	if err != nil || tok != "gho_real_oauth_token_123456" {
		t.Fatalf("PollToken failed: tok=%q err=%v", tok, err)
	}
}
