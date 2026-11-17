package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sync/errgroup"
)

// plan describes what sync would do with a single artifact.
type plan string

const (
	planCurrent  plan = "up to date"
	planCached   plan = "install from cache"
	planDownload plan = "download"
)

// probe is the outcome of validating a single artifact.
type probe struct {
	plan    plan
	problem error
}

// validate performs a dry run of sync: it resolves manifests, checks every
// active artifact the same way sync would and reports the planned action. It
// never downloads, extracts, installs or writes any file, and it collects all
// per-artifact problems instead of stopping at the first one.
//
// Manifests shipped inside artifacts are only visible once the artifact is
// installed, so recursion continues only into artifacts that are up to date.
func (e *engine) validate(ctx context.Context, vars map[string]any) error {
	g := newGraph(e.root, vars, e.state.Variables)
	paths := []string{filepath.Join(e.root, manifestName)}
	counts := map[plan]int{}
	var problems []error
	deferred := 0
	for len(paths) > 0 {
		batch, err := g.load(paths)
		if err != nil {
			return err
		}

		if err := g.checkRequired(); err != nil {
			problems = append(problems, err)
		}

		probes, err := e.probeAll(ctx, batch)
		if err != nil {
			return err
		}

		paths = nil
		for i, a := range batch {
			p := probes[i]
			if p.problem != nil {
				e.log.Error("artifact is invalid", "artifact", a.Name, "error", p.problem)
				problems = append(problems, fmt.Errorf("artifact %q: %w", a.Name, p.problem))
				continue
			}

			counts[p.plan]++
			rel, _ := filepath.Rel(e.root, a.dest)
			e.log.Info("artifact is valid", "artifact", a.Name, "to", rel, "plan", string(p.plan))

			if p.plan != planCurrent {
				deferred++
				continue
			}

			found, err := discover(a.dest)
			if err != nil {
				return err
			}

			g.inherit(a.dest, found, a.defaults)
			paths = append(paths, found...)
		}
	}

	if deferred > 0 {
		e.log.Warn("manifests inside artifacts that are not yet installed are validated during sync",
			"artifacts", deferred)
	}

	if len(problems) > 0 {
		return fmt.Errorf("validation found %d problem(s): %w", len(problems), errors.Join(problems...))
	}

	e.log.Info("validation passed",
		"artifacts", len(g.names),
		"up_to_date", counts[planCurrent],
		"from_cache", counts[planCached],
		"downloads", counts[planDownload])
	return nil
}

// probeAll checks a batch concurrently. Per-artifact problems are reported in
// the returned probes; the error is only set for cancellation or I/O failures
// that make the whole validation unreliable.
func (e *engine) probeAll(ctx context.Context, batch []artifact) ([]probe, error) {
	probes := make([]probe, len(batch))
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(e.jobs)

	for i, a := range batch {
		group.Go(func() error {
			p, err := e.probe(ctx, a)
			probes[i] = p
			return err
		})
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}

	return probes, nil
}

func (e *engine) probe(ctx context.Context, a artifact) (probe, error) {
	if a.SHA256 == "" {
		return probe{problem: errors.New("no sha256 checksum; sync refuses to download it")}, nil
	}

	if err := e.checkDestination(ctx, a); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return probe{}, ctxErr
		}

		return probe{problem: err}, nil
	}

	rel, _ := filepath.Rel(e.root, a.dest)
	if record, ok := e.state.Installed[rel]; ok && record.current(a) {
		// checkDestination already verified the installed tree against its record.
		if _, err := os.Lstat(a.dest); err == nil {
			return probe{plan: planCurrent}, nil
		}
	}

	cached, err := e.cache.has(ctx, a)
	if err != nil {
		return probe{}, err
	}

	if cached {
		if err := e.cache.checkStripPrefix(a); err != nil {
			return probe{problem: err}, nil
		}

		return probe{plan: planCached}, nil
	}

	return probe{plan: planDownload}, nil
}

// has reports whether materialize could serve a without downloading. Unlike
// materialize it is read-only: it neither takes locks nor repairs the cache.
func (c *cache) has(ctx context.Context, a artifact) (bool, error) {
	object := filepath.Join(c.dir, "objects", a.SHA256)
	if err := noSymlinks(c.dir, object); err != nil {
		return false, err
	}

	sum, err := checksum(ctx, filepath.Join(object, "source"))

	switch {
	case err == nil && sum == a.SHA256:
		return true, nil
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return false, err
	}

	format, err := archiveFormat(a)
	if err != nil {
		return false, err
	}

	if !unpacks(a, format) {
		// The original download is installed as-is, so only source counts.
		return false, nil
	}

	tree := filepath.Join(object, "unpacked-"+format)
	data, err := os.ReadFile(tree + ".json")
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	var record treeRecord
	if json.Unmarshal(data, &record) != nil || record.SHA256 != a.SHA256 || record.Format != format {
		return false, nil
	}

	if err := noSymlinks(c.dir, tree); err != nil {
		return false, err
	}

	sum, err = treeChecksum(ctx, tree)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}

		return false, nil
	}

	return sum == record.Tree, nil
}

// checkStripPrefix verifies a's strip_prefix against an already extracted
// tree. Without one, the prefix can only be checked by sync after extraction.
func (c *cache) checkStripPrefix(a artifact) error {
	if a.StripPrefix == "" {
		return nil
	}

	format, err := archiveFormat(a)
	if err != nil {
		return err
	}

	tree := filepath.Join(c.dir, "objects", a.SHA256, "unpacked-"+format)
	if _, err := os.Lstat(tree); err != nil {
		return nil
	}

	_, err = stripPrefix(tree, a)
	return err
}
