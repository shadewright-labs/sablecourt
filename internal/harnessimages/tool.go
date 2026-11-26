package harnessimages

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// ToolDir is where the harness container sees the tool volume, read-only.
const ToolDir = "/opt/harness-tool"

// Tool pins a vendor tool that the published image must not carry. It is
// downloaded on the user's machine from the vendor's own host, checked against
// the digests below, and stored in Volume, which no harness can write.
type Tool struct {
	// Binary is the file name in the tool volume and the command name.
	Binary  string
	Version string
	// Volume holds only the tool. It is separate from the login volume, so a
	// harness that can write its login state cannot replace its own binary.
	Volume string
	// URL is the vendor artefact. Its host must be in downloadHosts.
	URL string
	// ArchiveSHA512 and ArchiveMember are set when URL is a gzip tar archive:
	// the archive is checked first, then the one member is extracted.
	ArchiveSHA512 string
	ArchiveMember string
	// SHA256 and Size describe the executable itself. The install container
	// and every run check SHA256 again.
	SHA256 string
	Size   int64
}

// Downloader fetches one URL. Tests use a fake; HTTPDownloader is the real one.
type Downloader interface {
	Get(ctx context.Context, rawURL string) (io.ReadCloser, error)
}

// downloadHosts are the only hosts a tool is fetched from, one per vendor.
var downloadHosts = map[string]bool{
	"downloads.claude.ai":    true, // Anthropic's release bucket for Claude Code
	"storage.googleapis.com": true, // Google's public bucket for the Antigravity CLI
}

// maxArtefact bounds any download, archive or extracted binary.
const maxArtefact = 1 << 30

// HTTPDownloader fetches over HTTPS from a fixed host, follows no redirect,
// and sends no credentials or cookies.
type HTTPDownloader struct {
	// Client is replaced in tests. Its CheckRedirect is always overridden.
	Client *http.Client
}

var errRedirect = errors.New("the download host answered with a redirect; Sablecourt follows none")

func checkDownloadURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Port() != "" || !downloadHosts[u.Hostname()] {
		return errors.New("tool URL is not an HTTPS URL on a fixed vendor download host")
	}
	return nil
}

func (d HTTPDownloader) Get(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	if err := checkDownloadURL(rawURL); err != nil {
		return nil, err
	}
	c := http.Client{Timeout: 30 * time.Minute}
	if d.Client != nil {
		c = *d.Client
	}
	c.Jar = nil
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirect }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		if errors.Is(err, errRedirect) {
			return nil, errRedirect
		}
		return nil, fmt.Errorf("download %s: %w", rawURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("download %s: HTTP status %d", rawURL, resp.StatusCode)
	}
	return resp.Body, nil
}

// VerifiedTool is a downloaded executable whose SHA256 and size match the pin.
// Close removes it.
type VerifiedTool struct {
	Path string
	Tool Tool
	dir  string
}

func (v *VerifiedTool) Close() error {
	if v == nil || v.dir == "" {
		return nil
	}
	return os.RemoveAll(v.dir)
}

// Open returns the verified executable for streaming into the install container.
func (v *VerifiedTool) Open() (*os.File, error) { return os.Open(v.Path) }

// FetchTool downloads the pinned tool of role into a private temporary
// directory and checks it. Nothing unverified is returned.
func FetchTool(ctx context.Context, d Downloader, role string) (*VerifiedTool, error) {
	def, err := Lookup(role)
	if err != nil {
		return nil, err
	}
	if def.Tool == nil {
		return nil, fmt.Errorf("the %s image carries its tool; nothing to install", role)
	}
	return fetchTool(ctx, d, *def.Tool)
}

func fetchTool(ctx context.Context, d Downloader, pin Tool) (_ *VerifiedTool, err error) {
	if d == nil {
		return nil, errors.New("downloader is required")
	}
	if err := checkDownloadURL(pin.URL); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "sablecourt-tool-")
	if err != nil {
		return nil, fmt.Errorf("create tool download scratch: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	body, err := d.Get(ctx, pin.URL)
	if err != nil {
		return nil, fmt.Errorf("download %s %s: %w", pin.Binary, pin.Version, err)
	}
	defer func() { _ = body.Close() }()
	binary := filepath.Join(dir, pin.Binary)
	if pin.ArchiveSHA512 == "" {
		if err := writeChecked(binary, body, sha256.New(), pin.SHA256, pin.Size); err != nil {
			return nil, fmt.Errorf("%s %s: %w", pin.Binary, pin.Version, err)
		}
		return &VerifiedTool{Path: binary, Tool: pin, dir: dir}, nil
	}
	archive := filepath.Join(dir, "archive.tar.gz")
	if err := writeChecked(archive, body, sha512.New(), pin.ArchiveSHA512, -1); err != nil {
		return nil, fmt.Errorf("%s %s archive: %w", pin.Binary, pin.Version, err)
	}
	if err := extractMember(ctx, archive, pin, binary); err != nil {
		return nil, fmt.Errorf("%s %s: %w", pin.Binary, pin.Version, err)
	}
	if err := os.Remove(archive); err != nil {
		return nil, err
	}
	return &VerifiedTool{Path: binary, Tool: pin, dir: dir}, nil
}

var errChecksum = errors.New("checksum mismatch: the download is not the pinned artefact")

// writeChecked copies at most maxArtefact bytes into a new private file and
// compares the digest (and the size, when size >= 0).
func writeChecked(path string, r io.Reader, h hash.Hash, want string, size int64) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, maxArtefact+1))
	if err != nil {
		return fmt.Errorf("download interrupted: %w", err)
	}
	if n > maxArtefact {
		return errors.New("download is larger than any pinned artefact")
	}
	if (size >= 0 && n != size) || hex.EncodeToString(h.Sum(nil)) != want {
		return errChecksum
	}
	return nil
}

// extractMember reads the one named regular file out of a checked archive and
// checks its SHA256 and size. Other entries are skipped and never written.
func extractMember(ctx context.Context, archive string, pin Tool, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("read archive: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("archive has no %s", pin.ArchiveMember)
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if h.Name != pin.ArchiveMember {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return fmt.Errorf("archive entry %s is not a regular file", pin.ArchiveMember)
		}
		return writeChecked(dest, tr, sha256.New(), pin.SHA256, pin.Size)
	}
}
