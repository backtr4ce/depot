package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

const maxDownloadBytes int64 = 4 << 30

type cache struct {
	dir    string
	client *http.Client
	log    *slog.Logger
}

func checksum(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}

	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, contextReader{ctx, f}); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

func treeChecksum(ctx context.Context, root string) (string, error) {
	return treeChecksumExcept(ctx, root, nil)
}

func treeChecksumExcept(ctx context.Context, root string, excluded []string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		for _, skip := range excluded {
			if path == skip {
				if entry.IsDir() {
					return filepath.SkipDir
				}

				return nil
			}
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		rel, _ := filepath.Rel(root, path)

		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported file type at %s", path)
		}

		// Intermediate directories can be created by nested dependencies.
		// Files (including executable bits) and the root mode define integrity.
		if info.IsDir() && path != root {
			return nil
		}

		sum := ""
		if info.Mode().IsRegular() {
			sum, err = checksum(ctx, path)
			if err != nil {
				return err
			}
		}

		_, err = fmt.Fprintf(h, "%q %o %s\n", filepath.ToSlash(rel), info.Mode(), sum)
		return err
	})

	return hex.EncodeToString(h.Sum(nil)), err
}

func atomicJSON(path string, value any) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".json-*")
	if err != nil {
		return err
	}

	defer os.Remove(f.Name())

	err = json.NewEncoder(f).Encode(value)
	if err == nil {
		err = f.Sync()
	}

	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}

	return os.Rename(f.Name(), path)
}

func lock(ctx context.Context, path string) (*flock.Flock, error) {
	l := flock.New(path)
	ok, err := l.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, fmt.Errorf("could not acquire lock %s: %w", path, ctx.Err())
	}

	return l, nil
}

type treeRecord struct {
	SHA256 string `json:"sha256"`
	Format string `json:"format"`
	Tree   string `json:"tree"`
}

func (c *cache) materialize(ctx context.Context, a artifact) (string, error) {
	lockPath := filepath.Join(c.dir, "locks", a.SHA256+".lock")
	if err := noSymlinks(c.dir, lockPath); err != nil {
		return "", err
	}

	l, err := lock(ctx, lockPath)
	if err != nil {
		return "", err
	}

	defer l.Unlock()

	object := filepath.Join(c.dir, "objects", a.SHA256)
	if err := os.MkdirAll(object, 0o755); err != nil {
		return "", err
	}

	if err := noSymlinks(c.dir, object); err != nil {
		return "", err
	}

	source := filepath.Join(object, "source")
	format, _ := archiveFormat(a)
	preserve := !unpacks(a, format)
	tree := filepath.Join(object, "unpacked-"+format)
	recordPath := tree + ".json"
	if err := noSymlinks(c.dir, recordPath); err != nil {
		return "", err
	}

	if err := noSymlinks(c.dir, source); err != nil {
		return "", err
	}

	sourceOK := false
	if sum, err := checksum(ctx, source); err == nil {
		sourceOK = sum == a.SHA256
		if !sourceOK {
			c.log.Warn("discarding corrupt cached download", "artifact", a.Name)

			if err := os.Remove(source); err != nil {
				return "", err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	if !preserve {
		data, err := os.ReadFile(recordPath)
		var record treeRecord
		if err == nil && json.Unmarshal(data, &record) == nil && record.SHA256 == a.SHA256 && record.Format == format {
			if err := noSymlinks(c.dir, tree); err != nil {
				return "", err
			}

			sum, err := treeChecksum(ctx, tree)
			if err == nil && sum == record.Tree {
				c.log.Debug("using verified unpacked cache", "artifact", a.Name)
				return unpackedPath(tree, format), nil
			}

			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
	}

	if !sourceOK {
		if err := c.download(ctx, a, source); err != nil {
			return "", err
		}
	} else {
		c.log.Debug("using verified download cache", "artifact", a.Name)
	}

	if preserve {
		return source, nil
	}

	stage, err := os.MkdirTemp(object, ".extract-*")
	if err != nil {
		return "", err
	}

	defer os.RemoveAll(stage)

	if err := extract(ctx, source, stage, format); err != nil {
		return "", fmt.Errorf("extract: %w", err)
	}

	if err := os.Chmod(stage, 0o755); err != nil {
		return "", err
	}

	sum, err := treeChecksum(ctx, stage)
	if err != nil {
		return "", err
	}

	if err := noSymlinks(c.dir, tree); err != nil {
		return "", err
	}

	if err := os.RemoveAll(tree); err != nil {
		return "", err
	}

	if err := os.Rename(stage, tree); err != nil {
		return "", err
	}

	if err := atomicJSON(recordPath, treeRecord{a.SHA256, format, sum}); err != nil {
		return "", err
	}

	return unpackedPath(tree, format), nil
}

func unpackedPath(tree, format string) string {
	if format == "gzip" || format == "xz" {
		return filepath.Join(tree, "payload")
	}

	return tree
}

func (c *cache) download(ctx context.Context, a artifact, dest string) error {
	c.log.Info("downloading artifact", "artifact", a.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.From, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Accept-Encoding", "identity")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	if resp.ContentLength > maxDownloadBytes {
		return fmt.Errorf("download exceeds %d bytes", maxDownloadBytes)
	}

	f, err := os.CreateTemp(filepath.Dir(dest), ".download-*")
	if err != nil {
		return err
	}

	defer os.Remove(f.Name())

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(contextReader{ctx, resp.Body}, maxDownloadBytes+1))
	if err == nil && n > maxDownloadBytes {
		err = fmt.Errorf("download exceeds %d bytes", maxDownloadBytes)
	}

	if err == nil && hex.EncodeToString(h.Sum(nil)) != a.SHA256 {
		err = fmt.Errorf("sha256 mismatch for %q: expected %s, got %s", a.Name, a.SHA256, hex.EncodeToString(h.Sum(nil)))
	}

	if err == nil {
		err = f.Sync()
	}

	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}

	if err := os.Chmod(f.Name(), 0o644); err != nil {
		return err
	}

	return os.Rename(f.Name(), dest)
}
