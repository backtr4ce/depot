package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
)

// stampVersion changes whenever evaluation semantics change, so that state
// written by an older depot is never trusted.
const stampVersion = 1

// stamp records every input of a completed sync, like the stamp files of
// ninja and GN. While the stamp matches the workspace, state.json describes
// exactly the active artifacts and the manifests need not be evaluated again.
// Any edited, removed or added manifest makes the stamp stale.
type stamp struct {
	Version   int    `json:"version"`
	Variables string `json:"variables"`
	// Manifests maps every evaluated manifest to the SHA-256 of its contents.
	Manifests map[string]string `json:"manifests"`
	// Scans maps every searched directory to the manifests it contained,
	// except those inside other artifacts, which are covered by their own scan.
	Scans map[string][]string `json:"scans"`
}

func variablesDigest(vars map[string]any) (string, error) {
	// Maps are encoded with sorted keys, so the encoding is canonical.
	data, err := json.Marshal(vars)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// newStamp describes the inputs of a fully evaluated graph.
func newStamp(g *graph) (*stamp, error) {
	digest, err := variablesDigest(g.vars)
	if err != nil {
		return nil, err
	}

	destinations := make([]string, 0, len(g.targets))
	for dest := range g.targets {
		destinations = append(destinations, dest)
	}

	s := &stamp{
		Version:   stampVersion,
		Variables: digest,
		Manifests: map[string]string{},
		Scans:     map[string][]string{},
	}

	for path, sum := range g.digests {
		rel, _ := filepath.Rel(g.root, path)
		s.Manifests[rel] = sum
	}

	for dir, found := range g.scans {
		rel, _ := filepath.Rel(g.root, dir)
		s.Scans[rel] = scanResult(g.root, dir, found, destinations)
	}

	return s, nil
}

// scanResult returns the root-relative manifests in found that are not
// inside an artifact destination below dir.
func scanResult(root, dir string, found, destinations []string) []string {
	result := []string{}

	for _, path := range found {
		nested := false
		for _, dest := range destinations {
			if within(dest, path) && !within(dest, dir) {
				nested = true
				break
			}
		}

		if !nested {
			rel, _ := filepath.Rel(root, path)
			result = append(result, rel)
		}
	}

	sort.Strings(result)
	return result
}

// stale returns why the workspace no longer matches the stamp, or "" if the
// stamp is current. installed lists the recorded artifact destinations.
func (s *stamp) stale(ctx context.Context, root string, vars map[string]any, installed []string) (string, error) {
	if s == nil {
		return "no completed sync recorded", nil
	}

	if s.Version != stampVersion {
		return "state was written by a different depot version", nil
	}

	digest, err := variablesDigest(vars)
	if err != nil {
		return "", err
	}

	if digest != s.Variables {
		return "variables changed", nil
	}

	for _, rel := range sortedKeys(s.Manifests) {
		path, err := stampPath(root, rel)
		if err != nil {
			return "", err
		}

		if err := ctx.Err(); err != nil {
			return "", err
		}

		_, sum, err := readManifestBytes(path)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Sprintf("%s was removed", rel), nil
		}

		if err != nil {
			return "", err
		}

		if sum != s.Manifests[rel] {
			return fmt.Sprintf("%s changed", rel), nil
		}
	}

	destinations := make([]string, 0, len(installed))
	for _, rel := range installed {
		destinations = append(destinations, filepath.Join(root, rel))
	}

	for _, rel := range sortedKeys(s.Scans) {
		dir, err := stampPath(root, rel)
		if err != nil {
			return "", err
		}

		found, err := discover(dir)
		if err != nil {
			return "", err
		}

		if !slices.Equal(scanResult(root, dir, found, destinations), s.Scans[rel]) {
			return fmt.Sprintf("manifests were added or removed in %s", rel), nil
		}
	}

	return "", nil
}

// stampPath resolves a path read from state.json, which may have been edited.
func stampPath(root, rel string) (string, error) {
	if !filepath.IsLocal(rel) && rel != "." {
		return "", fmt.Errorf("invalid path %q in depot state", rel)
	}

	path := filepath.Join(root, rel)
	if err := noSymlinks(root, path); err != nil {
		return "", err
	}

	return path, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	sort.Strings(keys)
	return keys
}
