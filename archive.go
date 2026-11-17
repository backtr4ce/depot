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

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

const (
	maxExpandedBytes      int64 = 8 << 30
	maxArchiveFiles             = 100000
	maxSymlinkTargetBytes       = 4096
	maxSymlinkDepth             = 40

	// maxZstdWindow matches the largest window zstd --long creates by
	// default; it bounds decoder memory independently of the archive size.
	maxZstdWindow = 128 << 20
)

func extensionFormat(name string) string {
	name = strings.ToLower(name)

	for _, ext := range []string{"tar.gz", "tar.bz2", "tar.xz", "tar.zst", "tgz", "tbz2", "txz", "tzst", "zip", "tar", "gz", "xz", "zst"} {
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
	case "tzst":
		return "tar.zst"
	case "gz":
		return "gzip"
	case "zst":
		return "zstd"
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
	case "none", "tar", "tar.gz", "tar.bz2", "tar.xz", "tar.zst", "zip", "gzip", "xz", "zstd":
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
	case "tar", "tar.gz", "tar.bz2", "tar.xz", "tar.zst", "zip":
		return true
	}

	return false
}

// singleFile reports whether format is a compressed stream of one file.
func singleFile(format string) bool {
	return format == "gzip" || format == "xz" || format == "zstd"
}

// checkStripPrefix validates the syntax of a.StripPrefix and that it applies
// to an artifact that is extracted into a directory tree.
func checkStripPrefix(a artifact, format string) error {
	prefix := slashPath(a.StripPrefix)
	if prefix == "" {
		return nil
	}

	if !unpacks(a, format) || !multiFile(format) {
		return fmt.Errorf("strip_prefix requires a tar or zip archive that is extracted (format %q, to %q)",
			format, a.To)
	}

	clean := path.Clean(prefix)
	unsafe := strings.ContainsAny(prefix, ":\x00") ||
		path.IsAbs(prefix) ||
		clean == "." ||
		clean == ".." ||
		strings.HasPrefix(clean, "../")
	if unsafe {
		return fmt.Errorf("strip_prefix %q must be a relative path inside the archive", a.StripPrefix)
	}

	return nil
}

// stripPrefix returns the part of the extracted tree selected by a's
// strip_prefix, which may be a directory or a single file.
func stripPrefix(tree string, a artifact) (string, error) {
	if a.StripPrefix == "" {
		return tree, nil
	}

	selected := filepath.Join(tree, filepath.FromSlash(path.Clean(slashPath(a.StripPrefix))))
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
	links     []archiveSymlink
}

type archiveSymlink struct {
	name   string
	target string
}

func cleanArchivePath(name string, directory bool) (string, error) {
	normalized := slashPath(name)
	clean := path.Clean(normalized)
	if strings.ContainsAny(normalized, ":\x00") || path.IsAbs(normalized) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}

	if clean == "." && !directory {
		return "", fmt.Errorf("invalid archive file path %q", name)
	}

	return clean, nil
}

func validateSymlinkTarget(name, target string) error {
	if target == "" {
		return fmt.Errorf("symlink %q has an empty target", name)
	}
	if len(target) > maxSymlinkTargetBytes {
		return fmt.Errorf("symlink target for %q exceeds %d bytes", name, maxSymlinkTargetBytes)
	}
	if strings.ContainsAny(target, "\\:\x00") || path.IsAbs(target) {
		return fmt.Errorf("symlink %q target %q must be a relative portable path", name, target)
	}

	resolved := path.Clean(path.Join(path.Dir(name), target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") || path.IsAbs(resolved) ||
		!filepath.IsLocal(filepath.FromSlash(resolved)) {
		return fmt.Errorf("symlink %q target %q escapes the artifact", name, target)
	}

	return nil
}

func validateTreeSymlink(root, name, target string) error {
	rel, err := filepath.Rel(root, name)
	if err != nil {
		return err
	}

	name = filepath.ToSlash(rel)
	target = filepath.ToSlash(target)
	if err := validateSymlinkTarget(name, target); err != nil {
		return err
	}

	// Lexical cleaning alone misses escapes through a symlink followed by "..".
	current := []string{}
	if dir := path.Dir(name); dir != "." {
		current = strings.Split(dir, "/")
	}

	pending := strings.Split(target, "/")
	depth := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]

		switch part {
		case "", ".":
			continue
		case "..":
			if len(current) == 0 {
				return fmt.Errorf("symlink %q target %q escapes the artifact", name, target)
			}

			current = current[:len(current)-1]
			continue
		}

		current = append(current, part)
		candidate := filepath.Join(root, filepath.FromSlash(strings.Join(current, "/")))
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}

		depth++
		if depth > maxSymlinkDepth {
			return fmt.Errorf("symlink %q target exceeds %d link resolutions", name, maxSymlinkDepth)
		}

		linkTarget, err := os.Readlink(candidate)
		if err != nil {
			return err
		}
		linkTarget = filepath.ToSlash(linkTarget)
		linkName := strings.Join(current, "/")
		if err := validateSymlinkTarget(linkName, linkTarget); err != nil {
			return err
		}

		current = current[:len(current)-1]
		pending = append(strings.Split(linkTarget, "/"), pending...)
	}

	return nil
}

func (u *unpacker) symlink(name, target string) error {
	if err := u.ctx.Err(); err != nil {
		return err
	}

	clean, err := cleanArchivePath(name, false)
	if err != nil {
		return err
	}
	if !within(u.root, filepath.Join(u.root, filepath.FromSlash(clean))) {
		return fmt.Errorf("archive path escapes extraction directory: %q", name)
	}

	target = slashPath(target)
	if err := validateSymlinkTarget(clean, target); err != nil {
		return err
	}

	u.files++
	if u.files > maxArchiveFiles {
		return fmt.Errorf("archive exceeds %d entries", maxArchiveFiles)
	}

	u.remaining -= int64(len(target))
	if u.remaining < 0 {
		return fmt.Errorf("archive exceeds expanded size limit of %d bytes", maxExpandedBytes)
	}

	u.links = append(u.links, archiveSymlink{name: clean, target: target})
	return nil
}

func (u *unpacker) finish() error {
	for _, link := range u.links {
		if err := u.ctx.Err(); err != nil {
			return err
		}

		dest := filepath.Join(u.root, filepath.FromSlash(link.name))
		parent := filepath.Dir(dest)

		if err := noSymlinks(u.root, parent); err != nil {
			return err
		}
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return err
		}
		if err := noSymlinks(u.root, parent); err != nil {
			return err
		}
		if err := os.Symlink(link.target, dest); err != nil {
			return fmt.Errorf("create symlink %q: %w", link.name, err)
		}
	}

	for _, link := range u.links {
		dest := filepath.Join(u.root, filepath.FromSlash(link.name))
		if err := validateTreeSymlink(u.root, dest, link.target); err != nil {
			return fmt.Errorf("unsafe symlink %q: %w", link.name, err)
		}
	}

	return nil
}

func (u *unpacker) entry(name string, mode os.FileMode, r io.Reader, directory bool) error {
	if err := u.ctx.Err(); err != nil {
		return err
	}

	u.files++
	if u.files > maxArchiveFiles {
		return fmt.Errorf("archive exceeds %d entries", maxArchiveFiles)
	}

	clean, err := cleanArchivePath(name, directory)
	if err != nil {
		return err
	}
	if clean == "." {
		return nil
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
		if err != nil && (z == nil || !errors.Is(err, zip.ErrInsecurePath)) {
			return err
		}

		defer z.Close()

		for _, file := range z.File {
			entryType := file.Mode() & os.ModeType
			if entryType == os.ModeSymlink {
				r, err := file.Open()
				if err != nil {
					return err
				}

				target, readErr := io.ReadAll(io.LimitReader(
					contextReader{ctx, r},
					maxSymlinkTargetBytes+1))
				err = errors.Join(readErr, r.Close())
				if err != nil {
					return err
				}
				if len(target) > maxSymlinkTargetBytes {
					return fmt.Errorf("symlink target for %q exceeds %d bytes", file.Name, maxSymlinkTargetBytes)
				}
				if err := u.symlink(file.Name, string(target)); err != nil {
					return err
				}

				continue
			}

			if entryType != 0 && entryType != os.ModeDir {
				return fmt.Errorf("unsupported archive entry type for %q (links and devices are not allowed)", file.Name)
			}

			directory := entryType == os.ModeDir || strings.HasSuffix(file.Name, "\\")
			if directory {
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

		return u.finish()
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
	case "tar.zst", "zstd":
		zr, err := zstd.NewReader(r,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderLowmem(true),
			zstd.WithDecoderMaxWindow(maxZstdWindow))
		if err != nil {
			return err
		}

		defer zr.Close()

		r = zr
	}

	if singleFile(format) {
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

			if err != nil {
				return err
			}

			return u.finish()
		}

		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			err = u.entry(header.Name, os.FileMode(header.Mode), nil, true)
		case tar.TypeReg, tar.TypeRegA:
			err = u.entry(header.Name, os.FileMode(header.Mode), tr, false)
		case tar.TypeSymlink:
			err = u.symlink(header.Name, header.Linkname)
		default:
			return fmt.Errorf("unsupported archive entry type for %q (links and devices are not allowed)", header.Name)
		}

		if err != nil {
			return err
		}
	}
}
