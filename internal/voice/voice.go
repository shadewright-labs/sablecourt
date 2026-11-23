package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/secret"
)

const (
	DefaultSTTURL   = "http://127.0.0.1:8080/v1/audio/transcriptions"
	DefaultSTTModel = "whisper-large-v3-turbo"
	DefaultTTSModel = "kokoro"
)

type Options struct {
	Secrets    secret.SecretStore
	STTURL     string
	TTSURL     string
	STTModel   string
	TTSModel   string
	TTSVoice   string
	HTTPClient *http.Client
}

type Status struct {
	STTURL     string `json:"sttUrl"`
	STTModel   string `json:"sttModel"`
	TTSBackend string `json:"ttsBackend"`
	TTSURL     string `json:"ttsUrl,omitempty"`
	TTSModel   string `json:"ttsModel,omitempty"`
	TTSVoice   string `json:"ttsVoice,omitempty"`
}

// Service handles local-first Speech-to-Text and Text-to-Speech via OpenAI-compatible
// local servers (whisper.cpp, faster-whisper, mlx-audio, Kokoro, Piper) with native
// macOS `say` fallback for TTS.
type Service struct {
	secrets  secret.SecretStore
	sttURL   string
	ttsURL   string
	sttModel string
	ttsModel string
	ttsVoice string
	client   *http.Client
}

func New(opts Options) *Service {
	cli := opts.HTTPClient
	if cli == nil {
		cli = &http.Client{Timeout: 60 * time.Second}
	}
	return &Service{
		secrets:  opts.Secrets,
		sttURL:   strings.TrimSpace(opts.STTURL),
		ttsURL:   strings.TrimSpace(opts.TTSURL),
		sttModel: strings.TrimSpace(opts.STTModel),
		ttsModel: strings.TrimSpace(opts.TTSModel),
		ttsVoice: strings.TrimSpace(opts.TTSVoice),
		client:   cli,
	}
}

func (s *Service) resolve(ctx context.Context, explicit, secretKey, envKey, def string) string {
	if explicit != "" {
		return explicit
	}
	if s.secrets != nil && secretKey != "" {
		if v, err := s.secrets.Get(ctx, secretKey); err == nil && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if envKey != "" {
		if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
			return v
		}
	}
	return def
}

func (s *Service) Status(ctx context.Context) Status {
	sttURL := s.resolve(ctx, s.sttURL, "voice_stt_url", "SABLECOURT_STT_URL", DefaultSTTURL)
	sttModel := s.resolve(ctx, s.sttModel, "voice_stt_model", "SABLECOURT_STT_MODEL", DefaultSTTModel)
	ttsURL := s.resolve(ctx, s.ttsURL, "voice_tts_url", "SABLECOURT_TTS_URL", "")
	ttsModel := s.resolve(ctx, s.ttsModel, "voice_tts_model", "SABLECOURT_TTS_MODEL", DefaultTTSModel)
	ttsVoice := s.resolve(ctx, s.ttsVoice, "voice_tts_voice", "SABLECOURT_TTS_VOICE", "")

	backend := "unconfigured"
	if ttsURL != "" {
		backend = "http"
	} else if goruntime.GOOS == "darwin" {
		if _, err := exec.LookPath("say"); err == nil {
			backend = "macos-say"
		}
	}
	return Status{
		STTURL:     sttURL,
		STTModel:   sttModel,
		TTSBackend: backend,
		TTSURL:     ttsURL,
		TTSModel:   ttsModel,
		TTSVoice:   ttsVoice,
	}
}

// Transcribe sends audio bytes to the configured local OpenAI-compatible STT endpoint
// and returns the redacted transcript text.
func (s *Service) Transcribe(ctx context.Context, audio []byte, filename, language string) (string, error) {
	if len(audio) == 0 {
		return "", errors.New("audio payload is empty")
	}
	if strings.TrimSpace(filename) == "" {
		filename = "audio.webm"
	}
	sttURL := s.resolve(ctx, s.sttURL, "voice_stt_url", "SABLECOURT_STT_URL", DefaultSTTURL)
	sttModel := s.resolve(ctx, s.sttModel, "voice_stt_model", "SABLECOURT_STT_MODEL", DefaultSTTModel)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", filepath.Base(filename))
	if err != nil {
		return "", err
	}
	if _, err := part.Write(audio); err != nil {
		return "", err
	}
	_ = mw.WriteField("model", sttModel)
	if strings.TrimSpace(language) != "" {
		_ = mw.WriteField("language", strings.TrimSpace(language))
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sttURL, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("local STT request to %s failed: %w", sttURL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("local STT returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var parsed struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parsed); err == nil && parsed.Text != "" {
		return redact.String(strings.TrimSpace(parsed.Text)), nil
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return "", errors.New("empty transcript returned by STT")
	}
	return redact.String(text), nil
}

// Speak synthesizes speech audio for text using either the configured local OpenAI-compatible
// TTS server or native macOS `say` (outputting standard WAV audio).
func (s *Service) Speak(ctx context.Context, text, voiceOverride string) ([]byte, string, error) {
	clean := redact.String(strings.TrimSpace(text))
	if clean == "" {
		return nil, "", errors.New("text is required")
	}
	if len(clean) > 4000 {
		clean = clean[:4000]
	}

	ttsURL := s.resolve(ctx, s.ttsURL, "voice_tts_url", "SABLECOURT_TTS_URL", "")
	voiceName := strings.TrimSpace(voiceOverride)
	if voiceName == "" {
		voiceName = s.resolve(ctx, s.ttsVoice, "voice_tts_voice", "SABLECOURT_TTS_VOICE", "")
	}

	if ttsURL != "" {
		ttsModel := s.resolve(ctx, s.ttsModel, "voice_tts_model", "SABLECOURT_TTS_MODEL", DefaultTTSModel)
		if voiceName == "" {
			voiceName = "af_heart"
		}
		payload, _ := json.Marshal(map[string]string{
			"model":           ttsModel,
			"input":           clean,
			"voice":           voiceName,
			"response_format": "wav",
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ttsURL, bytes.NewReader(payload))
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.client.Do(req)
		if err != nil {
			return nil, "", fmt.Errorf("local TTS request to %s failed: %w", ttsURL, err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			return nil, "", err
		}
		if resp.StatusCode >= 400 {
			return nil, "", fmt.Errorf("local TTS returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "audio/wav"
		}
		return raw, ct, nil
	}

	if goruntime.GOOS == "darwin" {
		if sayBin, err := exec.LookPath("say"); err == nil {
			tmpFile, err := os.CreateTemp("", "sablecourt-tts-*.wav")
			if err != nil {
				return nil, "", err
			}
			tmpPath := tmpFile.Name()
			_ = tmpFile.Close()
			defer os.Remove(tmpPath)

			args := []string{"-o", tmpPath, "--file-format=WAVE", "--data-format=LEI16@22050"}
			if voiceName != "" {
				args = append(args, "-v", voiceName)
			}
			args = append(args, "--", clean)
			cmd := exec.CommandContext(ctx, sayBin, args...)
			if out, err := cmd.CombinedOutput(); err != nil {
				return nil, "", fmt.Errorf("macos say failed: %s (%w)", strings.TrimSpace(string(out)), err)
			}
			wavBytes, err := os.ReadFile(tmpPath)
			if err != nil {
				return nil, "", err
			}
			return wavBytes, "audio/wav", nil
		}
	}

	return nil, "", errors.New("no local TTS backend configured; set SABLECOURT_TTS_URL (Kokoro/Piper/mlx-audio)")
}
