package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// obsolete lists inactive managed destinations only after the complete graph
// is known. Local edits and active descendants make automatic removal unsafe.
func (e *engine) obsolete(ctx context.Context, g *graph) ([]string, error) {
	var paths []string

	for rel := range e.state.Installed {
		dest, err := workspacePath(e.root, rel)
		if err != nil {
			return nil, err
		}

		if dest == e.root {
			return nil, fmt.Errorf("refusing to remove the workspace root")
		}

		if _, active := g.targets[dest]; active {
			continue
		}

		for active := range g.targets {
			if within(dest, active) {
				return nil, fmt.Errorf("cannot remove inactive artifact %s containing active destination %s; move it aside first",
					rel, active)
			}
		}

		if err := e.checkRecorded(ctx, dest); err != nil {
			return nil, err
		}

		paths = append(paths, rel)
	}

	// Retire nested destinations before their ancestors.
	sort.Sort(sort.Reverse(sort.StringSlice(paths)))
	return paths, nil
}

func (e *engine) retire(ctx context.Context, rel string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	dest := filepath.Join(e.root, rel)
	if err := e.checkRecorded(ctx, dest); err != nil {
		return err
	}

	var stage string
	defer func() {
		if stage != "" {
			os.RemoveAll(stage)
		}
	}()

	if _, err := os.Lstat(dest); err == nil {
		stage, err = os.MkdirTemp(filepath.Dir(dest), ".depot-remove-*")
		if err != nil {
			return err
		}

		if err := os.Rename(dest, filepath.Join(stage, "previous")); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	old := e.state.Installed[rel]
	delete(e.state.Installed, rel)

	if err := atomicJSON(e.statePath, e.state); err != nil {
		e.state.Installed[rel] = old
		if stage != "" {
			backup := filepath.Join(stage, "previous")
			if restoreErr := os.Rename(backup, dest); restoreErr != nil {
				stage = ""
				return errors.Join(err, fmt.Errorf("restore backup %s: %w", backup, restoreErr))
			}
		}

		return err
	}

	e.log.Info("removed inactive artifact", "to", rel)
	return nil
}
