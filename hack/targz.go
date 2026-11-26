//go:build ignore

// Command targz packs the regular files of one directory into a gzip-compressed tar archive whose bytes depend
// only on the files and the Go toolchain: entries sorted by name at the archive root, owner 0:0 with no user or
// group name, mode 0755, every time set to the given epoch, the USTAR format, and a gzip header with no file name
// and the same epoch. hack/package-binaries.sh runs it with `go run`, so the release archives use the same Go
// toolchain as the binaries and no other compressor.
//
// Usage: go run hack/targz.go <source dir> <archive> <epoch seconds>
package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: go run hack/targz.go <source dir> <archive> <epoch seconds>")
		os.Exit(2)
	}
	epoch, err := strconv.ParseInt(os.Args[3], 10, 64)
	if err != nil || epoch < 0 {
		fmt.Fprintf(os.Stderr, "epoch %q is not a number of seconds\n", os.Args[3])
		os.Exit(2)
	}
	if err := pack(os.Args[1], os.Args[2], time.Unix(epoch, 0).UTC()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func pack(src, dest string, when time.Time) (err error) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.Type().IsRegular() {
			return fmt.Errorf("%s: only regular files can be packed", filepath.Join(src, e.Name()))
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	gz, err := gzip.NewWriterLevel(out, gzip.BestCompression)
	if err != nil {
		return err
	}
	gz.ModTime = when
	tw := tar.NewWriter(gz)
	for _, name := range names {
		if err := add(tw, filepath.Join(src, name), name, when); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func add(tw *tar.Writer, path, name string, when time.Time) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Size:     info.Size(),
		Mode:     0o755,
		ModTime:  when,
		Format:   tar.FormatUSTAR,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	n, err := io.Copy(tw, f)
	if err != nil {
		return err
	}
	if n != info.Size() {
		return fmt.Errorf("%s changed while it was packed", path)
	}
	return nil
}
