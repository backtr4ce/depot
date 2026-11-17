package main

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ulikunitz/xz"
)

const (
	maxExpandedBytes int64 = 8 << 30
	maxArchiveFiles        = 100000
)

func extensionFormat(name string) string {
	name = strings.ToLower(name)

	for _, ext := range []string{"tar.gz", "tar.bz2", "tar.xz", "tgz", "tbz2", "txz", "zip", "tar", "gz", "xz"} {
		if strings.HasSuffix(name, "."+ext) {
			return normalizeFormat(ext)
		}
	}

	return "none"
}

func normalizeFormat(format string) string {
	switch format {
	case "tgz":
		return "tar.gz"
	case "tbz2":
		return "tar.bz2"
	case "txz":
		return "tar.xz"
	case "gz":
		return "gzip"
	}

	return format
}

func archiveFormat(a artifact) (string, error) {
	if a.Archive == "" {
		u, err := url.Parse(a.From)
		if err != nil {
			return "", err
		}

		return extensionFormat(u.Path), nil
	}

	format := normalizeFormat(strings.ToLower(a.Archive))

	switch format {
	case "none", "tar", "tar.gz", "tar.bz2", "tar.xz", "zip", "gzip", "xz":
		return format, nil
	default:
		return "", fmt.Errorf("unsupported archive format %q", a.Archive)
	}
}

// unpacks reports whether an artifact in the given format is extracted. Plain
// files, and archives whose destination itself names an archive, are
// installed byte-for-byte.
func unpacks(a artifact, format string) bool {
	return format != "none" && extensionFormat(a.To) == "none"
}

// multiFile reports whether format extracts to a directory tree rather than
// a single decompressed payload.
func multiFile(format string) bool {
	switch format {
	case "tar", "tar.gz", "tar.bz2", "tar.xz", "zip":
		return true
	}

	return false
}

// checkStripPrefix validates the syntax of a.StripPrefix and that it applies
// to an artifact that is extracted into a directory tree.
func checkStripPrefix(a artifact, format string) error {
	prefix := a.StripPrefix
	if prefix == "" {
		return nil
	}

	if !unpacks(a, format) || !multiFile(format) {
		return fmt.Errorf("strip_prefix requires a tar or zip archive that is extracted (format %q, to %q)",
			format, a.To)
	}

	clean := path.Clean(prefix)
	unsafe := strings.ContainsAny(prefix, "\\:\x00") ||
		path.IsAbs(prefix) ||
		clean == "." ||
		clean == ".." ||
		strings.HasPrefix(clean, "../")
	if unsafe {
		return fmt.Errorf("strip_prefix %q must be a relative path inside the archive", prefix)
	}

	return nil
}

// stripPrefix returns the part of the extracted tree selected by a's
// strip_prefix, which may be a directory or a single file.
func stripPrefix(tree string, a artifact) (string, error) {
	if a.StripPrefix == "" {
		return tree, nil
	}

	selected := filepath.Join(tree, filepath.FromSlash(path.Clean(a.StripPrefix)))
	if !within(tree, selected) {
		return "", fmt.Errorf("strip_prefix %q escapes the archive", a.StripPrefix)
	}

	if err := noSymlinks(tree, selected); err != nil {
		return "", err
	}

	_, err := os.Lstat(selected)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return "", fmt.Errorf("strip_prefix %q not found in archive; top-level entries: %s",
			a.StripPrefix, topLevelEntries(tree))
	}

	if err != nil {
		return "", err
	}

	return selected, nil
}

// topLevelEntries lists the root of an extracted tree to help fix a wrong
// strip_prefix.
func topLevelEntries(tree string) string {
	const limit = 10
	entries, err := os.ReadDir(tree)
	if err != nil {
		return "<unreadable>"
	}

	var names []string
	for i, entry := range entries {
		if i == limit {
			names = append(names, "...")
			break
		}

		name := entry.Name()

		if entry.IsDir() {
			name += "/"
		}

		names = append(names, name)
	}

	if len(names) == 0 {
		return "<empty>"
	}

	return strings.Join(names, ", ")
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}

	return r.r.Read(p)
}

type unpacker struct {
	ctx       context.Context
	root      string
	remaining int64
	files     int
}

func (u *unpacker) entry(name string, mode os.FileMode, r io.Reader, directory bool) error {
	if err := u.ctx.Err(); err != nil {
		return err
	}

	u.files++
	if u.files > maxArchiveFiles {
		return fmt.Errorf("archive exceeds %d entries", maxArchiveFiles)
	}

	// Backslashes and drive prefixes are rejected even on Unix so an archive
	// has the same containment rules on every supported host.
	clean := path.Clean(name)
	if strings.ContainsAny(name, "\\:\x00") || path.IsAbs(name) || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("unsafe archive path %q", name)
	}

	if clean == "." {
		if directory {
			return nil
		}

		return fmt.Errorf("invalid archive file path %q", name)
	}

	dest := filepath.Join(u.root, filepath.FromSlash(clean))
	if !within(u.root, dest) {
		return fmt.Errorf("archive path escapes extraction directory: %q", name)
	}

	if directory {
		return os.MkdirAll(dest, 0o755)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644|mode.Perm()&0o111)
	if err != nil {
		return err
	}

	n, copyErr := io.Copy(f, io.LimitReader(contextReader{u.ctx, r}, u.remaining+1))
	closeErr := f.Close()
	u.remaining -= n
	if u.remaining < 0 {
		return fmt.Errorf("archive exceeds expanded size limit of %d bytes", maxExpandedBytes)
	}

	return errors.Join(copyErr, closeErr)
}

func extract(ctx context.Context, source, dest, format string) error {
	u := &unpacker{ctx: ctx, root: dest, remaining: maxExpandedBytes}

	if format == "zip" {
		z, err := zip.OpenReader(source)
		if err != nil {
			return err
		}

		defer z.Close()

		for _, file := range z.File {
			if file.Mode()&os.ModeType != 0 && !file.FileInfo().IsDir() {
				return fmt.Errorf("unsupported archive entry type for %q (links and devices are not allowed)", file.Name)
			}

			if file.FileInfo().IsDir() {
				if err := u.entry(file.Name, file.Mode(), nil, true); err != nil {
					return err
				}

				continue
			}

			r, err := file.Open()
			if err != nil {
				return err
			}

			err = u.entry(file.Name, file.Mode(), r, false)
			err = errors.Join(err, r.Close())
			if err != nil {
				return err
			}
		}

		return nil
	}

	f, err := os.Open(source)
	if err != nil {
		return err
	}

	defer f.Close()

	var r io.Reader = contextReader{ctx, f}

	switch format {
	case "tar.gz", "gzip":
		gz, err := gzip.NewReader(r)
		if err != nil {
			return err
		}

		defer gz.Close()

		r = gz
	case "tar.bz2":
		r = bzip2.NewReader(r)
	case "tar.xz", "xz":
		r, err = xz.NewReader(r)
		if err != nil {
			return err
		}
	}

	if format == "gzip" || format == "xz" {
		return u.entry("payload", 0o644, r, false)
	}

	tr := tar.NewReader(r)

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			// Validate compression trailers as well as tar entries, with a bound
			// on trailing data to avoid decompression bombs after the tar EOF.
			n, err := io.Copy(io.Discard, io.LimitReader(contextReader{ctx, r}, u.remaining+1))
			if n > u.remaining {
				return fmt.Errorf("archive exceeds expanded size limit")
			}

			return err
		}

		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			err = u.entry(header.Name, os.FileMode(header.Mode), nil, true)
		case tar.TypeReg, tar.TypeRegA:
			err = u.entry(header.Name, os.FileMode(header.Mode), tr, false)
		default:
			return fmt.Errorf("unsupported archive entry type for %q (links and devices are not allowed)", header.Name)
		}

		if err != nil {
			return err
		}
	}
}
