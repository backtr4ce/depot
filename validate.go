package main

import (
	"context"
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
	reason, err := e.state.Stamp.stale(ctx, e.root, vars, sortedKeys(e.state.Installed))
	if err != nil {
		return err
	}

	if reason == "" {
		e.log.Info("manifests are unchanged since the last sync")
	} else {
		e.log.Info("sync will evaluate manifests", "reason", reason)
	}

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
				if !a.IgnoreDepot {
					deferred++
				}
				continue
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

	if deferred > 0 {
		e.log.Warn("manifests inside artifacts that are not yet installed are validated during sync",
			"artifacts", deferred)
	} else if len(problems) == 0 {
		obsolete, err := e.obsolete(ctx, g)
		if err != nil {
			problems = append(problems, err)
		} else {
			for _, rel := range obsolete {
				e.log.Info("inactive artifact", "to", rel, "plan", "remove")
			}
		}
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
		return probe{plan: planCached}, nil
	}

	return probe{plan: planDownload}, nil
}

// has checks the original download without taking locks or repairing the cache.
func (c *cache) has(ctx context.Context, a artifact) (bool, error) {
	object := filepath.Join(c.dir, "objects", a.SHA256)
	if err := noSymlinks(c.dir, object); err != nil {
		return false, err
	}

	sum, err := checksum(ctx, filepath.Join(object, "source"))

	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}

	return err == nil && sum == a.SHA256, nil
}
