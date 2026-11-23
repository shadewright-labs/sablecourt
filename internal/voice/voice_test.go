package voice

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTranscribeAndSpeakViaLocalHTTP(t *testing.T) {
	cli := &http.Client{
		Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			switch {
			case strings.Contains(r.URL.Path, "/transcriptions"):
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"text":"Deploy with token sk-1234567890abcdefghij123456"}`)),
				}, nil
			case strings.Contains(r.URL.Path, "/speech"):
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"audio/wav"}},
					Body:       io.NopCloser(strings.NewReader("RIFFfakewav")),
				}, nil
			default:
				return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		}),
	}

	svc := New(Options{
		STTURL:     "http://127.0.0.1:8080/v1/audio/transcriptions",
		TTSURL:     "http://127.0.0.1:8880/v1/audio/speech",
		HTTPClient: cli,
	})

	text, err := svc.Transcribe(context.Background(), []byte("fake-audio"), "clip.webm", "pl")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if strings.Contains(text, "sk-1234567890") {
		t.Fatalf("expected secret redaction in transcript, got %q", text)
	}
	if !strings.Contains(text, "Deploy with token") {
		t.Fatalf("unexpected transcript: %q", text)
	}

	audio, ct, err := svc.Speak(context.Background(), "Conclave active", "")
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	if ct != "audio/wav" || string(audio) != "RIFFfakewav" {
		t.Fatalf("unexpected TTS output: ct=%q len=%d", ct, len(audio))
	}
}
