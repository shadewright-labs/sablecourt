package harnessimages

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

type fakeDownloader struct {
	body []byte
	err  error
	urls []string
}

func (f *fakeDownloader) Get(_ context.Context, u string) (io.ReadCloser, error) {
	f.urls = append(f.urls, u)
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(bytes.NewReader(f.body)), nil
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

type truncatedDownloader struct{}

func (truncatedDownloader) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(io.MultiReader(strings.NewReader("partial"), failingReader{})), nil
}

func sum256(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func sum512(b []byte) string { s := sha512.Sum512(b); return hex.EncodeToString(s[:]) }

func tarGz(t *testing.T, entries map[string][]byte, link string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if link != "" {
		if err := tw.WriteHeader(&tar.Header{Name: link, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFetchTool(t *testing.T) {
	binary := []byte("#!/bin/sh\necho pinned\n")
	other := []byte("#!/bin/sh\necho other version\n")
	direct := Tool{Binary: "claude", Version: "1.0.0", URL: "https://downloads.claude.ai/claude-code-releases/1.0.0/linux-arm64/claude", SHA256: sum256(binary), Size: int64(len(binary))}
	good := tarGz(t, map[string][]byte{"antigravity": binary, "README": []byte("x")}, "")
	archived := Tool{Binary: "agy", Version: "1.0.0", URL: "https://storage.googleapis.com/antigravity-public/x.tar.gz", ArchiveSHA512: sum512(good), ArchiveSize: int64(len(good)), ArchiveMember: "antigravity", SHA256: sum256(binary), Size: int64(len(binary))}
	wrongMember := tarGz(t, map[string][]byte{"antigravity": other}, "")
	noMember := tarGz(t, map[string][]byte{"other": binary}, "")
	linkMember := tarGz(t, nil, "antigravity")
	withPin := func(base Tool, archive []byte) Tool {
		base.ArchiveSHA512, base.ArchiveSize = sum512(archive), int64(len(archive))
		return base
	}

	for _, tc := range []struct {
		name    string
		pin     Tool
		dl      Downloader
		wantErr string
	}{
		{name: "direct binary matches the pin", pin: direct, dl: &fakeDownloader{body: binary}},
		{name: "archive and member match the pins", pin: archived, dl: &fakeDownloader{body: good}},
		{name: "download failure", pin: direct, dl: &fakeDownloader{err: errors.New("connection refused")}, wantErr: "connection refused"},
		{name: "interrupted download", pin: direct, dl: truncatedDownloader{}, wantErr: "download interrupted"},
		{name: "direct checksum mismatch", pin: direct, dl: &fakeDownloader{body: other}, wantErr: "checksum mismatch"},
		{name: "direct wrong size with right prefix", pin: direct, dl: &fakeDownloader{body: append(append([]byte(nil), binary...), 'x')}, wantErr: "checksum mismatch"},
		{name: "archive checksum mismatch", pin: archived, dl: &fakeDownloader{body: wrongMember}, wantErr: "archive: checksum mismatch"},
		{name: "archive pinned but member is another version", pin: withPin(archived, wrongMember), dl: &fakeDownloader{body: wrongMember}, wantErr: "checksum mismatch"},
		{name: "archive without the member", pin: withPin(archived, noMember), dl: &fakeDownloader{body: noMember}, wantErr: "archive has no antigravity"},
		{name: "member is a symlink", pin: withPin(archived, linkMember), dl: &fakeDownloader{body: linkMember}, wantErr: "not a regular file"},
		{name: "host not allowed", pin: Tool{Binary: "claude", URL: "https://example.invalid/claude", SHA256: direct.SHA256, Size: direct.Size}, dl: &fakeDownloader{body: binary}, wantErr: "fixed vendor download host"},
		{name: "plain http", pin: Tool{Binary: "claude", URL: "http://downloads.claude.ai/claude", SHA256: direct.SHA256, Size: direct.Size}, dl: &fakeDownloader{body: binary}, wantErr: "fixed vendor download host"},
		{name: "no downloader", pin: direct, wantErr: "downloader is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := tempEntries(t)
			v, err := fetchTool(t.Context(), tc.dl, tc.pin)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || v != nil {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if after := tempEntries(t); after != before {
					t.Fatalf("a failed fetch left %d scratch directories", after-before)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(v.Path)
			if err != nil || !bytes.Equal(got, binary) {
				t.Fatalf("verified file = %q, %v", got, err)
			}
			if err := v.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(v.Path); !os.IsNotExist(err) {
				t.Fatal("Close kept the download")
			}
		})
	}
}

// endlessDownloader serves far more bytes than any pin and counts what was read.
type endlessDownloader struct{ read int64 }

func (e *endlessDownloader) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(&countingReader{n: &e.read, left: 16 << 20}), nil
}

type countingReader struct {
	n    *int64
	left int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.left == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	clear(p)
	c.left -= int64(len(p))
	*c.n += int64(len(p))
	return len(p), nil
}

// A hostile or broken server must not make sablectl read or write more than
// one byte past the pinned size of the artefact it asked for.
func TestFetchToolReadsAtMostThePinnedSize(t *testing.T) {
	binary := []byte("#!/bin/sh\necho pinned\n")
	archive := tarGz(t, map[string][]byte{"antigravity": binary}, "")
	for _, pin := range []Tool{
		{Binary: "claude", Version: "1.0.0", URL: "https://downloads.claude.ai/x/claude", SHA256: sum256(binary), Size: int64(len(binary))},
		{Binary: "agy", Version: "1.0.0", URL: "https://storage.googleapis.com/x.tar.gz", ArchiveSHA512: sum512(archive), ArchiveSize: int64(len(archive)),
			ArchiveMember: "antigravity", SHA256: sum256(binary), Size: int64(len(binary))},
	} {
		t.Run(pin.Binary, func(t *testing.T) {
			limit := pin.Size
			if pin.ArchiveSHA512 != "" {
				limit = pin.ArchiveSize
			}
			dl := &endlessDownloader{}
			if _, err := fetchTool(t.Context(), dl, pin); !errors.Is(err, errChecksum) {
				t.Fatalf("err = %v, want a checksum mismatch", err)
			}
			if dl.read > limit+1 {
				t.Fatalf("read %d bytes for a pinned size of %d", dl.read, limit)
			}
		})
	}
}

func TestFetchToolRefusesPinWithoutSize(t *testing.T) {
	binary := []byte("x")
	for name, pin := range map[string]Tool{
		"binary size":  {Binary: "claude", URL: "https://downloads.claude.ai/x/claude", SHA256: sum256(binary)},
		"archive size": {Binary: "agy", URL: "https://storage.googleapis.com/x.tar.gz", ArchiveSHA512: sum512(binary), ArchiveMember: "antigravity", SHA256: sum256(binary), Size: 1},
	} {
		t.Run(name, func(t *testing.T) {
			dl := &fakeDownloader{body: binary}
			if _, err := fetchTool(t.Context(), dl, pin); err == nil || !strings.Contains(err.Error(), "pinned size") || len(dl.urls) != 0 {
				t.Fatalf("err = %v, urls %v", err, dl.urls)
			}
		})
	}
}

func tempEntries(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "sablecourt-tool-") {
			n++
		}
	}
	return n
}

func TestFetchToolRoles(t *testing.T) {
	if _, err := FetchTool(t.Context(), &fakeDownloader{}, "codex"); err == nil || !strings.Contains(err.Error(), "carries its tool") {
		t.Fatalf("codex: %v", err)
	}
	for _, role := range []string{"claude", "agy"} {
		d, _ := Lookup(role)
		if d.Tool == nil || d.Tool.Version != d.Version || d.Tool.Volume == d.Volume || d.Tool.Binary != role ||
			len(d.Tool.SHA256) != 64 || d.Tool.Size <= 0 || checkDownloadURL(d.Tool.URL) != nil {
			t.Fatalf("%s pin: %+v", role, d.Tool)
		}
		dl := &fakeDownloader{body: []byte("not the tool")}
		if _, err := FetchTool(t.Context(), dl, role); !errors.Is(err, errChecksum) || len(dl.urls) != 1 || dl.urls[0] != d.Tool.URL {
			t.Fatalf("%s: err=%v urls=%v", role, err, dl.urls)
		}
	}
	if d, _ := Lookup("agy"); d.Tool.ArchiveSHA512 == "" || d.Tool.ArchiveSize <= 0 || d.Tool.ArchiveMember != "antigravity" {
		t.Fatal("agy must keep the archive SHA-512 and size pins")
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHTTPDownloader(t *testing.T) {
	ok := "https://downloads.claude.ai/claude-code-releases/1.0.0/linux-arm64/claude"
	for _, tc := range []struct {
		name, url, wantErr string
		status             int
		location           string
	}{
		{name: "fixed host", url: ok, status: 200},
		{name: "redirect to another host", url: ok, status: 302, location: "https://attacker.invalid/claude", wantErr: "redirect"},
		{name: "redirect on the same host", url: ok, status: 301, location: ok + "2", wantErr: "redirect"},
		{name: "not found", url: ok, status: 404, wantErr: "HTTP status 404"},
		{name: "other host", url: "https://example.invalid/x", wantErr: "fixed vendor download host"},
		{name: "explicit port", url: "https://downloads.claude.ai:8443/x", wantErr: "fixed vendor download host"},
		{name: "credentials in URL", url: (&url.URL{Scheme: "https", User: url.UserPassword("user", "pw"), Host: "downloads.claude.ai", Path: "/x"}).String(), wantErr: "fixed vendor download host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests int
			client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				requests++
				if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Error("credentials sent")
				}
				h := http.Header{}
				if tc.location != "" {
					h.Set("Location", tc.location)
				}
				return &http.Response{StatusCode: tc.status, Header: h, Body: io.NopCloser(strings.NewReader("body")), Request: r}, nil
			})}
			body, err := HTTPDownloader{Client: client}.Get(t.Context(), tc.url)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v", err)
				}
				if requests > 1 {
					t.Fatal("a redirect was followed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(body)
			_ = body.Close()
			if string(got) != "body" || requests != 1 {
				t.Fatalf("body %q, requests %d", got, requests)
			}
		})
	}
}
