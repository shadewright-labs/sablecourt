package voice

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/secret"
)

// TestVoiceRefusesPlainHTTPBackends: recorded audio and the text to speak
// leave the daemon, so a voice backend URL that is plain http off the machine
// is refused before a request, from every source (option, secret, variable),
// and the error names the source, never the URL.
func TestVoiceRefusesPlainHTTPBackends(t *testing.T) {
	cred := "hunter" + "2secret"
	type src struct{ opt, secretVal, env string }
	cases := []struct {
		name  string
		stt   bool
		src   src
		field string // "" means sent
	}{
		{"stt loopback default", true, src{}, ""},
		{"stt https option", true, src{opt: "https://stt.example.test/v1/audio/transcriptions"}, ""},
		{"stt loopback v6 variable", true, src{env: "http://[::1]:8080/inference"}, ""},
		{"stt option", true, src{opt: "http://u:" + cred + "@stt.example.test/inference"}, "voice STT URL"},
		{"stt secret", true, src{secretVal: "http://stt.example.test/inference"}, "voice_stt_url"},
		{"stt variable", true, src{env: "ftp://stt.example.test/inference"}, "SABLECOURT_STT_URL"},
		{"tts https option", false, src{opt: "https://tts.example.test/v1/audio/speech"}, ""},
		{"tts loopback secret", false, src{secretVal: "http://127.0.0.1:8880/v1/audio/speech"}, ""},
		{"tts option", false, src{opt: "http://tts.example.test/v1/audio/speech"}, "voice TTS URL"},
		{"tts secret", false, src{secretVal: "http://u:" + cred + "@tts.example.test/v1/audio/speech"}, "voice_tts_url"},
		{"tts variable", false, src{env: "http://10.1.2.3/v1/audio/speech"}, "SABLECOURT_TTS_URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SABLECOURT_STT_URL", "")
			t.Setenv("SABLECOURT_TTS_URL", "")
			var calls int
			cli := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"audio/wav"}}, Body: io.NopCloser(strings.NewReader(`{"text":"hello"}`)), Request: r}, nil
			})}
			opts := Options{HTTPClient: cli}
			secrets := map[string]string{}
			key, env := "voice_tts_url", "SABLECOURT_TTS_URL"
			if tc.stt {
				key, env = "voice_stt_url", "SABLECOURT_STT_URL"
				opts.STTURL = tc.src.opt
			} else {
				opts.TTSURL = tc.src.opt
			}
			if tc.src.secretVal != "" {
				secrets[key] = tc.src.secretVal
			}
			if tc.src.env != "" {
				t.Setenv(env, tc.src.env)
			}
			opts.Secrets = secret.NewMemoryStore(secrets)
			svc := New(opts)
			var err error
			if tc.stt {
				_, err = svc.Transcribe(context.Background(), []byte("audio"), "a.wav", "")
			} else {
				_, _, err = svc.Speak(context.Background(), "hello", "")
			}
			if tc.field == "" {
				if err != nil || calls != 1 {
					t.Fatalf("err = %v after %d calls, want one request", err, calls)
				}
				return
			}
			if err == nil || !strings.HasPrefix(err.Error(), tc.field+": ") || strings.Contains(err.Error(), cred) || strings.Contains(err.Error(), "example.test") {
				t.Fatalf("err = %v, want a refusal naming %s without the URL", err, tc.field)
			}
			if calls != 0 {
				t.Fatalf("%d requests sent, want none", calls)
			}
			st := svc.Status(context.Background())
			if strings.Contains(st.STTURL+st.TTSURL, cred) {
				t.Fatalf("Status shows credentials: %+v", st)
			}
		})
	}
}
