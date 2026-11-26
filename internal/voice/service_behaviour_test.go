package voice

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/secret"
)

func handlerClient(h http.Handler) *http.Client {
	return &http.Client{
		Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			return rec.Result(), nil
		}),
	}
}

func TestStatusAndConfigPrecedence(t *testing.T) {
	ctx := context.Background()

	t.Run("explicit overrides secret overrides env overrides defaults", func(t *testing.T) {
		t.Setenv("SABLECOURT_STT_URL", "http://127.0.0.1:9001")
		t.Setenv("SABLECOURT_STT_MODEL", "env-stt")
		t.Setenv("SABLECOURT_TTS_URL", "http://127.0.0.1:9002/v1/audio/speech")
		t.Setenv("SABLECOURT_TTS_MODEL", "env-tts")
		t.Setenv("SABLECOURT_TTS_VOICE", "env-voice")

		secrets := secret.NewMemoryStore(map[string]string{
			"voice_stt_url":   "http://127.0.0.1:9101/inference",
			"voice_stt_model": "secret-stt",
			"voice_tts_voice": "secret-voice",
		})

		svc := New(Options{
			Secrets:  secrets,
			TTSModel: "explicit-tts",
		})
		st := svc.Status(ctx)
		if st.STTURL != "http://127.0.0.1:9101/inference" {
			t.Errorf("STTURL = %q, want secret /inference URL preserved", st.STTURL)
		}
		if st.STTModel != "secret-stt" {
			t.Errorf("STTModel = %q, want secret-stt", st.STTModel)
		}
		if st.TTSBackend != "http" || st.TTSURL != "http://127.0.0.1:9002/v1/audio/speech" {
			t.Errorf("TTS backend=%q url=%q", st.TTSBackend, st.TTSURL)
		}
		if st.TTSModel != "explicit-tts" || st.TTSVoice != "secret-voice" {
			t.Errorf("TTS model=%q voice=%q", st.TTSModel, st.TTSVoice)
		}
	})

	t.Run("URL suffix normalization and backend detection", func(t *testing.T) {
		t.Setenv("SABLECOURT_STT_URL", "")
		t.Setenv("SABLECOURT_TTS_URL", "")
		emptyDir := t.TempDir()
		t.Setenv("PATH", emptyDir)

		svc := New(Options{STTURL: "http://127.0.0.1:8080/"})
		st := svc.Status(ctx)
		if st.STTURL != "http://127.0.0.1:8080/v1/audio/transcriptions" {
			t.Errorf("normalized STTURL = %q", st.STTURL)
		}
		if st.TTSBackend != "unconfigured" {
			t.Errorf("TTSBackend without say or URL = %q, want unconfigured", st.TTSBackend)
		}

		if goruntime.GOOS == "darwin" {
			fakeSay := filepath.Join(emptyDir, "say")
			if err := os.WriteFile(fakeSay, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			st2 := svc.Status(ctx)
			if st2.TTSBackend != "macos-say" {
				t.Errorf("TTSBackend with say in PATH = %q, want macos-say", st2.TTSBackend)
			}
		}
	})
}

func TestTranscribeBehavioursAndErrors(t *testing.T) {
	ctx := context.Background()
	tok := "sk-" + strings.Repeat("a", 28)

	t.Run("multipart fields, URL normalization and response modes", func(t *testing.T) {
		cases := []struct {
			name         string
			sttURL       string
			wantPath     string
			filename     string
			wantFile     string
			language     string
			wantLang     string
			respStatus   int
			respBody     string
			wantText     string
			wantErr      string
			wantRedacted bool
		}{
			{
				name:       "base URL appends path and defaults filename",
				sttURL:     "http://127.0.0.1:8080",
				wantPath:   "/v1/audio/transcriptions",
				filename:   "   ",
				wantFile:   "audio.webm",
				respStatus: http.StatusOK,
				respBody:   `{"text":"  hello world  "}`,
				wantText:   "hello world",
			},
			{
				name:         "inference URL kept, basename extracted and plain text redacted",
				sttURL:       "http://127.0.0.1:8080/inference",
				wantPath:     "/inference",
				filename:     "/nested/dir/recording.wav",
				wantFile:     "recording.wav",
				language:     " en ",
				wantLang:     "en",
				respStatus:   http.StatusOK,
				respBody:     "  plain transcript with " + tok + "  ",
				wantText:     "plain transcript with",
				wantRedacted: true,
			},
			{
				name:       "HTTP 502 error propagates body",
				sttURL:     "http://127.0.0.1:8080/v1/audio/transcriptions",
				wantPath:   "/v1/audio/transcriptions",
				filename:   "a.wav",
				wantFile:   "a.wav",
				respStatus: http.StatusBadGateway,
				respBody:   "whisper overloaded",
				wantErr:    "HTTP 502: whisper overloaded",
			},
			{
				name:       "empty body refused",
				sttURL:     "http://127.0.0.1:8080/v1/audio/transcriptions",
				wantPath:   "/v1/audio/transcriptions",
				filename:   "a.wav",
				wantFile:   "a.wav",
				respStatus: http.StatusOK,
				respBody:   "   \n\t ",
				wantErr:    "empty transcript",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var gotPath, gotFile, gotModel, gotLang string
				var gotAudio []byte
				cli := handlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					gotPath = r.URL.Path
					mr, err := r.MultipartReader()
					if err != nil {
						t.Errorf("MultipartReader: %v", err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					for {
						part, err := mr.NextPart()
						if errors.Is(err, io.EOF) {
							break
						}
						if err != nil {
							t.Errorf("NextPart: %v", err)
							break
						}
						data, _ := io.ReadAll(part)
						switch part.FormName() {
						case "file":
							gotFile = part.FileName()
							gotAudio = data
						case "model":
							gotModel = string(data)
						case "language":
							gotLang = string(data)
						}
					}
					w.WriteHeader(tc.respStatus)
					_, _ = w.Write([]byte(tc.respBody))
				}))

				svc := New(Options{STTURL: tc.sttURL, STTModel: "whisper-test", HTTPClient: cli})
				got, err := svc.Transcribe(ctx, []byte("RIFF-data"), tc.filename, tc.language)
				if tc.wantErr != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
						t.Fatalf("Transcribe = %q, %v; want %q", got, err, tc.wantErr)
					}
					return
				}
				if err != nil {
					t.Fatalf("Transcribe unexpected err: %v", err)
				}
				if gotPath != tc.wantPath || gotFile != tc.wantFile || gotModel != "whisper-test" || gotLang != tc.wantLang || string(gotAudio) != "RIFF-data" {
					t.Fatalf("request path=%q file=%q model=%q lang=%q audio=%q", gotPath, gotFile, gotModel, gotLang, gotAudio)
				}
				if !strings.Contains(got, tc.wantText) {
					t.Fatalf("transcript = %q, want %q", got, tc.wantText)
				}
				if tc.wantRedacted && strings.Contains(got, tok) {
					t.Fatalf("transcript leaked secret: %q", got)
				}
			})
		}
	})

	t.Run("empty audio, invalid URL and transport error", func(t *testing.T) {
		svc := New(Options{})
		if _, err := svc.Transcribe(ctx, nil, "a.wav", ""); err == nil || !strings.Contains(err.Error(), "empty") {
			t.Fatalf("empty audio = %v", err)
		}
		badURL := New(Options{STTURL: "://bad"})
		if _, err := badURL.Transcribe(ctx, []byte("x"), "a.wav", ""); err == nil {
			t.Fatal("invalid STT URL succeeded")
		}
		failClient := &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		})}
		netErrSvc := New(Options{STTURL: "http://127.0.0.1:8080/v1/audio/transcriptions", HTTPClient: failClient})
		if _, err := netErrSvc.Transcribe(ctx, []byte("x"), "a.wav", ""); err == nil || !strings.Contains(err.Error(), "connection refused") {
			t.Fatalf("transport error = %v", err)
		}
	})
}

func TestSpeakBehavioursAndFallbacks(t *testing.T) {
	ctx := context.Background()
	tok := "sk-" + strings.Repeat("b", 28)

	t.Run("HTTP TTS redacts secrets, truncates at 4000 chars, handles voice and content-type defaults", func(t *testing.T) {
		var gotPayload map[string]string
		var respCT string
		var respStatus int
		var respBody string
		cli := handlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPayload = nil
			_ = json.NewDecoder(r.Body).Decode(&gotPayload)
			if respCT != "" {
				w.Header().Set("Content-Type", respCT)
			}
			w.WriteHeader(respStatus)
			_, _ = w.Write([]byte(respBody))
		}))

		svc := New(Options{
			TTSURL:     "http://127.0.0.1:8880/v1/audio/speech",
			TTSModel:   "kokoro-v1",
			HTTPClient: cli,
		})

		// Empty text refused.
		if _, _, err := svc.Speak(ctx, "   ", ""); err == nil || !strings.Contains(err.Error(), "text is required") {
			t.Fatalf("blank text Speak = %v", err)
		}

		// Redaction, truncation and default voice + default audio/wav content-type.
		respCT = ""
		respStatus = http.StatusOK
		respBody = "WAVDATA"
		longInput := "secret " + tok + " " + strings.Repeat("z", 4500)
		audio, ct, err := svc.Speak(ctx, longInput, "")
		if err != nil {
			t.Fatalf("Speak: %v", err)
		}
		if string(audio) != "WAVDATA" || ct != "audio/wav" {
			t.Fatalf("audio=%q ct=%q", audio, ct)
		}
		if gotPayload["model"] != "kokoro-v1" || gotPayload["voice"] != "af_heart" || gotPayload["response_format"] != "wav" {
			t.Fatalf("payload metadata = %+v", gotPayload)
		}
		if strings.Contains(gotPayload["input"], tok) {
			t.Fatal("TTS payload leaked secret")
		}
		if len(gotPayload["input"]) != 4000 {
			t.Fatalf("truncated input len = %d, want 4000", len(gotPayload["input"]))
		}

		// Voice override and custom Content-Type header.
		respCT = "audio/mpeg"
		_, ct, err = svc.Speak(ctx, "hello", "bf_emma")
		if err != nil || ct != "audio/mpeg" || gotPayload["voice"] != "bf_emma" {
			t.Fatalf("override err=%v ct=%q voice=%q", err, ct, gotPayload["voice"])
		}

		// HTTP error status propagates message.
		respStatus = http.StatusServiceUnavailable
		respBody = "tts busy"
		if _, _, err := svc.Speak(ctx, "hello", ""); err == nil || !strings.Contains(err.Error(), "HTTP 503: tts busy") {
			t.Fatalf("HTTP 503 Speak = %v", err)
		}

		// Invalid URL and transport failure.
		badSvc := New(Options{TTSURL: "://bad"})
		if _, _, err := badSvc.Speak(ctx, "hello", ""); err == nil {
			t.Fatal("invalid TTS URL succeeded")
		}
		failSvc := New(Options{
			TTSURL: "http://127.0.0.1:8880/v1/audio/speech",
			HTTPClient: &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("dial failed")
			})},
		})
		if _, _, err := failSvc.Speak(ctx, "hello", ""); err == nil || !strings.Contains(err.Error(), "dial failed") {
			t.Fatalf("transport error Speak = %v", err)
		}
	})

	t.Run("unconfigured backend and macOS say fallback", func(t *testing.T) {
		t.Setenv("SABLECOURT_TTS_URL", "")
		t.Setenv("SABLECOURT_TTS_VOICE", "")
		binDir := t.TempDir()
		t.Setenv("PATH", binDir)

		svc := New(Options{})
		if _, _, err := svc.Speak(ctx, "hello", ""); err == nil || !strings.Contains(err.Error(), "no local TTS backend configured") {
			t.Fatalf("unconfigured Speak = %v", err)
		}

		if goruntime.GOOS != "darwin" {
			return
		}
		argsFile := filepath.Join(t.TempDir(), "say-args.txt")
		script := "#!/bin/sh\n" +
			"out=\"\"\n" +
			"prev=\"\"\n" +
			"for a in \"$@\"; do\n" +
			"  printf '%s\\n' \"$a\" >> \"" + argsFile + "\"\n" +
			"  if [ \"$prev\" = \"-o\" ]; then out=\"$a\"; fi\n" +
			"  prev=\"$a\"\n" +
			"done\n" +
			"printf 'RIFF-FAKE-SAY' > \"$out\"\n"
		sayPath := filepath.Join(binDir, "say")
		if err := os.WriteFile(sayPath, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		audio, ct, err := svc.Speak(ctx, "hello "+tok, "Samantha")
		if err != nil || ct != "audio/wav" || string(audio) != "RIFF-FAKE-SAY" {
			t.Fatalf("macos say Speak = %q, %q, %v", audio, ct, err)
		}
		logged, err := os.ReadFile(argsFile)
		if err != nil {
			t.Fatal(err)
		}
		argStr := string(logged)
		if !strings.Contains(argStr, "-v\nSamantha\n") || strings.Contains(argStr, tok) {
			t.Fatalf("say args = %q", argStr)
		}

		// Failing say binary returns error with stderr output.
		failScript := "#!/bin/sh\necho 'voice not found' >&2\nexit 1\n"
		if err := os.WriteFile(sayPath, []byte(failScript), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := svc.Speak(ctx, "hello", "Missing"); err == nil || !strings.Contains(err.Error(), "macos say failed: voice not found") {
			t.Fatalf("failing say Speak = %v", err)
		}
	})
}
