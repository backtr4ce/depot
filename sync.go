package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

type installation struct {
	SHA256      string `json:"sha256"`
	StripPrefix string `json:"strip_prefix,omitempty"`
	Format      string `json:"format,omitempty"`
	Tree        string `json:"tree"`
}

// current reports whether the record describes the installed form of a.
func (i installation) current(a artifact) bool {
	return i.SHA256 == a.SHA256 && i.StripPrefix == a.StripPrefix && i.Format == installedFormat(a)
}

func installedFormat(a artifact) string {
	format, _ := archiveFormat(a)
	if !unpacks(a, format) {
		return "none"
	}

	return format
}

type state struct {
	Variables map[string]string       `json:"variables"`
	Installed map[string]installation `json:"installed"`
	// Stamp is only set after a sync completed; see stamp.
	Stamp *stamp `json:"stamp,omitempty"`
}

func readState(path string) (*state, error) {
	s := &state{}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	if err == nil {
		if err := json.Unmarshal(data, s); err != nil {
			return nil, fmt.Errorf("invalid depot state %s: %w", path, err)
		}
	}

	if s.Variables == nil {
		s.Variables = map[string]string{}
	}

	if s.Installed == nil {
		s.Installed = map[string]installation{}
	}

	return s, nil
}

type engine struct {
	root      string
	statePath string
	state     *state
	cache     *cache
	jobs      int
	log       *slog.Logger
}

func (e *engine) sync(ctx context.Context, vars map[string]any) error {
	current, err := e.upToDate(ctx, vars)
	if err != nil {
		return err
	}

	if current {
		e.log.Info("workspace is up to date", "artifacts", len(e.state.Installed))
		return nil
	}

	// Until the new evaluation completes, state.json no longer describes the
	// active artifacts; every state write below persists that.
	e.state.Stamp = nil

	stage, err := os.MkdirTemp(e.cache.dir, ".sync-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	g := newGraph(e.root, vars, e.state.Variables)
	paths := []string{filepath.Join(e.root, manifestName)}

	for len(paths) > 0 {
		batch, err := g.load(paths)
		if err != nil {
			return err
		}

		if err := g.checkRequired(); err != nil {
			return err
		}

		if missing := missingChecksums(batch); len(missing) > 0 {
			return fmt.Errorf("refusing to download artifacts without a sha256 checksum: %s", strings.Join(missing, ", "))
		}

		for _, a := range batch {
			if err := e.checkDestination(ctx, a); err != nil {
				return err
			}
		}

		sources, err := e.prepare(ctx, batch, stage)
		if err != nil {
			return err
		}

		paths = nil
		for i, a := range batch {
			if sources[i] != "" {
				if err := e.install(ctx, a, sources[i]); err != nil {
					return fmt.Errorf("install %q: %w", a.Name, err)
				}
			}

			if a.IgnoreDepot {
				continue
			}

			found, err := g.discover(a.dest)
			if err != nil {
				return err
			}

			g.inherit(a.dest, found, a.defaults)
			paths = append(paths, found...)
		}
	}

	if err := g.checkRequired(); err != nil {
		return err
	}

	obsolete, err := e.obsolete(ctx, g)
	if err != nil {
		return err
	}

	for _, rel := range obsolete {
		if err := e.retire(ctx, rel); err != nil {
			return err
		}
	}

	e.state.Stamp, err = newStamp(g)
	if err != nil {
		return err
	}

	if err := atomicJSON(e.statePath, e.state); err != nil {
		e.state.Stamp = nil
		return err
	}

	e.log.Info("workspace synchronized", "artifacts", len(g.names))
	return nil
}

// upToDate reports whether the stamp shows that no manifest needs evaluating
// and every recorded artifact is still installed unmodified.
func (e *engine) upToDate(ctx context.Context, vars map[string]any) (bool, error) {
	installed := sortedKeys(e.state.Installed)
	reason, err := e.state.Stamp.stale(ctx, e.root, vars, installed)
	if err != nil {
		return false, err
	}

	if reason != "" {
		e.log.Info("evaluating manifests", "reason", reason)
		return false, nil
	}

	statePath, _ := filepath.Rel(e.root, e.statePath)
	e.log.Info("manifests unchanged since last sync; using state instead of evaluating",
		"state", filepath.ToSlash(statePath),
		"manifests", len(e.state.Stamp.Manifests),
		"artifacts", len(installed))

	for _, rel := range installed {
		dest, err := workspacePath(e.root, rel)
		if err != nil {
			return false, err
		}

		if _, err := os.Lstat(dest); errors.Is(err, os.ErrNotExist) {
			e.log.Info("evaluating manifests", "reason", fmt.Sprintf("%s is missing", rel))
			return false, nil
		} else if err != nil {
			return false, err
		}

		if err := e.checkRecorded(ctx, dest); err != nil {
			return false, err
		}

		e.log.Debug("verified installed artifact from state", "to", rel)
	}

	return true, nil
}

func (e *engine) prepare(ctx context.Context, batch []artifact, stage string) ([]string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sources := make([]string, len(batch))
	workers := min(e.jobs, len(batch))
	work := make(chan int)
	var wg sync.WaitGroup
	var once sync.Once
	var failure error
	for range workers {
		wg.Go(func() {
			for i := range work {
				source, err := e.prepareArtifact(ctx, batch[i], stage)
				if err != nil {
					once.Do(func() {
						failure = fmt.Errorf("artifact %q: %w", batch[i].Name, err)
						cancel()
					})

					return
				}

				sources[i] = source
			}
		})
	}

send:
	for i := range batch {
		select {
		case work <- i:
		case <-ctx.Done():
			break send
		}
	}

	close(work)
	wg.Wait()

	if failure != nil {
		return nil, failure
	}

	return sources, ctx.Err()
}

// prepareArtifact skips verified current installations. Otherwise it downloads
// the original bytes and extracts into temporary storage owned by this sync.
func (e *engine) prepareArtifact(ctx context.Context, a artifact, stage string) (string, error) {
	rel, _ := filepath.Rel(e.root, a.dest)
	if record, ok := e.state.Installed[rel]; ok && record.current(a) {
		if _, err := os.Lstat(a.dest); err == nil {
			lockPath := filepath.Join(e.cache.dir, "locks", a.SHA256+".lock")
			if err := noSymlinks(e.cache.dir, lockPath); err != nil {
				return "", err
			}

			l, err := lock(ctx, lockPath)
			if err != nil {
				return "", err
			}
			defer l.Unlock()

			return "", e.cache.removeLegacyTrees(a)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}

	source, err := e.cache.downloadSource(ctx, a)
	if err != nil {
		return "", err
	}

	format := installedFormat(a)
	if format == "none" {
		return source, nil
	}

	tree, err := os.MkdirTemp(stage, "extract-*")
	if err != nil {
		return "", err
	}

	if err := extract(ctx, source, tree, format); err != nil {
		return "", fmt.Errorf("extract: %w", err)
	}

	if err := os.Chmod(tree, 0o755); err != nil {
		return "", err
	}

	return stripPrefix(unpackedPath(tree, format), a)
}

func (e *engine) checkDestination(ctx context.Context, a artifact) error {
	if err := e.checkRecorded(ctx, a.dest); err != nil {
		return err
	}

	for rel := range e.state.Installed {
		child := filepath.Join(e.root, rel)
		if child != a.dest && within(a.dest, child) {
			if err := e.checkRecorded(ctx, child); err != nil {
				return err
			}
		}
	}

	return nil
}

func (e *engine) checkRecorded(ctx context.Context, dest string) error {
	if err := noSymlinks(e.root, dest); err != nil {
		return err
	}

	if _, err := os.Lstat(dest); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}

	rel, _ := filepath.Rel(e.root, dest)
	record, managed := e.state.Installed[rel]
	if !managed {
		return fmt.Errorf("refusing to overwrite unmanaged destination %s; move it aside first", rel)
	}

	var excluded []string
	for path := range e.state.Installed {
		child := filepath.Join(e.root, path)
		if child != dest && within(dest, child) {
			excluded = append(excluded, child)
		}
	}

	sum, err := treeChecksumExcept(ctx, dest, excluded)
	if err != nil {
		return err
	}

	if sum != record.Tree {
		return fmt.Errorf("refusing to overwrite locally modified artifact at %s; move it aside first", rel)
	}

	return nil
}

func copyTree(ctx context.Context, source, dest string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		rel, _ := filepath.Rel(source, path)
		target := dest
		if rel != "." {
			target = filepath.Join(dest, rel)
		}

		if info.IsDir() {
			return os.Mkdir(target, info.Mode().Perm())
		}

		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported file type at %s", path)
		}

		in, err := os.Open(path)
		if err != nil {
			return err
		}

		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			in.Close()
			return err
		}

		_, err = io.Copy(out, contextReader{ctx, in})
		if err == nil {
			err = out.Sync()
		}

		return errors.Join(err, in.Close(), out.Close())
	})
}

func (e *engine) install(ctx context.Context, a artifact, source string) error {
	if err := e.checkDestination(ctx, a); err != nil {
		return err
	}

	sum, err := treeChecksum(ctx, source)
	if err != nil {
		return err
	}

	rel, _ := filepath.Rel(e.root, a.dest)

	for path := range e.state.Installed {
		child := filepath.Join(e.root, path)
		if child != a.dest && within(a.dest, child) {
			nested, _ := filepath.Rel(a.dest, child)
			if _, err := os.Lstat(filepath.Join(source, nested)); err == nil {
				return fmt.Errorf("parent archive contains managed child destination %s", path)
			} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
				return err
			}
		}
	}

	old, hadOld := e.state.Installed[rel]
	if old.current(a) && old.Tree == sum {
		if _, err := os.Lstat(a.dest); err == nil {
			e.log.Debug("artifact is current", "artifact", a.Name)
			return nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(a.dest), 0o755); err != nil {
		return err
	}

	stage, err := os.MkdirTemp(filepath.Dir(a.dest), ".depot-install-*")
	if err != nil {
		return err
	}

	defer func() {
		if stage != "" {
			os.RemoveAll(stage)
		}
	}()

	payload := filepath.Join(stage, "payload")
	if err := copyTree(ctx, source, payload); err != nil {
		return err
	}

	copied, err := treeChecksum(ctx, payload)
	if err != nil {
		return err
	}

	if copied != sum {
		return fmt.Errorf("staged artifact failed integrity verification")
	}

	if err := e.checkDestination(ctx, a); err != nil {
		return err
	}

	backup := filepath.Join(stage, "previous")
	moved := false
	if _, err := os.Lstat(a.dest); err == nil {
		if err := os.Rename(a.dest, backup); err != nil {
			return err
		}

		moved = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	rollback := func(cause error, installed bool) error {
		if installed {
			if err := os.RemoveAll(a.dest); err != nil {
				return errors.Join(cause, fmt.Errorf("rollback: %w", err))
			}
		}

		if moved {
			if err := os.Rename(backup, a.dest); err != nil {
				// Keep the backup if rollback fails; never delete the last copy.
				stage = ""
				return errors.Join(cause, fmt.Errorf("restore backup %s: %w", backup, err))
			}
		}

		return cause
	}

	if err := os.Rename(payload, a.dest); err != nil {
		return rollback(err, false)
	}

	e.state.Installed[rel] = installation{
		SHA256:      a.SHA256,
		StripPrefix: a.StripPrefix,
		Format:      installedFormat(a),
		Tree:        sum,
	}

	if err := atomicJSON(e.statePath, e.state); err != nil {
		if hadOld {
			e.state.Installed[rel] = old
		} else {
			delete(e.state.Installed, rel)
		}

		return rollback(err, true)
	}

	e.log.Info("installed artifact", "artifact", a.Name, "to", rel)
	return nil
}
