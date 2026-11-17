package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func runClean(ctx context.Context, s *session) error {
	e := &engine{
		root:  s.root,
		state: s.state,
	}
	var destinations []string

	// Check every destination before deleting anything, including recorded
	// artifacts no longer mentioned by the current manifests.
	for rel := range s.state.Installed {
		dest, err := workspacePath(s.root, rel)
		if err != nil {
			return err
		}

		if dest == s.root {
			return fmt.Errorf("refusing to clean the workspace root")
		}

		if err := e.checkRecorded(ctx, dest); err != nil {
			return err
		}

		destinations = append(destinations, dest)
	}

	objects := filepath.Join(s.cacheDir, "objects")
	if err := noSymlinks(s.root, objects); err != nil {
		return err
	}

	entries, err := os.ReadDir(s.cacheDir)
	if err != nil {
		return err
	}

	cachePaths := []string{objects}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".sync-") || strings.HasPrefix(entry.Name(), ".json-") {
			cachePaths = append(cachePaths, filepath.Join(s.cacheDir, entry.Name()))
		}
	}

	// Remove nested destinations before their parents.
	sort.Sort(sort.Reverse(sort.StringSlice(destinations)))
	for _, dest := range append(destinations, cachePaths...) {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := os.RemoveAll(dest); err != nil {
			return err
		}
	}

	s.log.Info("workspace cleaned", "artifacts", len(destinations))
	return nil
}
