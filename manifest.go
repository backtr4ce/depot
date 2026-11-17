package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	manifestName    = "DEPOT.yaml"
	maxManifestSize = 4 << 20
)

type manifest struct {
	Vars      map[string]any `yaml:"vars,omitempty"`
	Includes  []string       `yaml:"includes"`
	Artifacts []artifact     `yaml:"artifacts"`
	Required  []string       `yaml:"required"`
}

type artifact struct {
	Name      string `yaml:"name"`
	To        string `yaml:"to"`
	From      string `yaml:"from"`
	Condition string `yaml:"condition"`
	SHA256    string `yaml:"sha256"`
	Archive   string `yaml:"archive"`
	// StripPrefix selects a slash-separated subpath of an extracted archive;
	// only that directory or file is installed at To.
	StripPrefix string `yaml:"strip_prefix"`
	IgnoreDepot bool   `yaml:"ignore_depot,omitempty"`
	dest        string
	defaults    map[string]any
}

// readManifest parses the manifest at path and returns it together with the
// SHA-256 of the bytes that were parsed.
func readManifest(path string) (manifest, string, error) {
	var m manifest
	data, sum, err := readManifestBytes(path)
	if err != nil {
		return m, "", err
	}

	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)

	if err := d.Decode(&m); err != nil {
		return m, "", fmt.Errorf("%s: %w", path, err)
	}

	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return m, "", fmt.Errorf("%s: expected exactly one YAML document", path)
	}

	return m, sum, nil
}

// readManifestBytes returns the manifest at path with \r\n and \r line
// endings converted to \n, and the SHA-256 of the converted bytes. Hashing
// after conversion keeps the stamp valid when a checkout only changes line
// endings.
func readManifestBytes(path string) ([]byte, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}

	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxManifestSize+1))
	if err != nil {
		return nil, "", err
	}

	if len(data) > maxManifestSize {
		return nil, "", fmt.Errorf("%s: manifest exceeds 4 MiB", path)
	}

	data = normalizeNewlines(data)
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

func normalizeNewlines(data []byte) []byte {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && filepath.IsLocal(rel)
}

func workspacePath(root, path string) (string, error) {
	if !filepath.IsLocal(path) {
		return "", fmt.Errorf("path %q must be relative and cannot escape the workspace root", path)
	}

	full := filepath.Clean(filepath.Join(root, path))
	if !within(root, full) {
		return "", fmt.Errorf("path %q escapes workspace", path)
	}

	rel, _ := filepath.Rel(root, full)

	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == ".git" || part == ".cache" {
			return "", fmt.Errorf("path %q uses a reserved directory", path)
		}
	}

	if err := noSymlinks(root, full); err != nil {
		return "", err
	}

	return full, nil
}

func noSymlinks(root, path string) error {
	if !within(root, path) {
		return fmt.Errorf("%s is outside workspace", path)
	}

	rel, _ := filepath.Rel(root, path)
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		if err != nil {
			return err
		}

		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink path %s", current)
		}
	}

	return nil
}

type graph struct {
	root      string
	vars      map[string]any
	overrides map[string]string
	inherited map[string]inheritance
	loaded    map[string]map[string]any
	seen      map[string]bool
	names     map[string]string
	targets   map[string]string
	required  map[string]bool
	ignored   []string
	// digests and scans are the inputs of the evaluation; see stamp.
	digests map[string]string
	scans   map[string][]string
}

func newGraph(root string, vars map[string]any, overrides map[string]string) *graph {
	return &graph{
		root:      root,
		vars:      vars,
		overrides: overrides,
		inherited: map[string]inheritance{},
		loaded:    map[string]map[string]any{},
		seen:      map[string]bool{},
		names:     map[string]string{},
		targets:   map[string]string{},
		required:  map[string]bool{},
		digests:   map[string]string{},
		scans:     map[string][]string{},
	}
}

// inheritance records where a manifest was discovered: the directory that was
// searched and the defaults of the manifest (or artifact) that caused the search.
type inheritance struct {
	scope    string
	defaults map[string]any
}

// inheritedDefaults returns the defaults of the nearest already loaded manifest
// in an ancestor directory within the discovery scope, falling back to the
// defaults of the manifest that triggered discovery. This lets an intermediate
// manifest pass its defaults to deeper manifests found by the same recursive
// search.
func (g *graph) inheritedDefaults(path string) map[string]any {
	origin, ok := g.inherited[path]
	if !ok {
		return nil
	}

	for dir := filepath.Dir(filepath.Dir(path)); within(origin.scope, dir) || dir == origin.scope; dir = filepath.Dir(dir) {
		if defaults, ok := g.loaded[filepath.Join(dir, manifestName)]; ok {
			return defaults
		}

		if dir == origin.scope {
			break
		}
	}

	return origin.defaults
}

func (g *graph) manifestVariables(path string, local map[string]any) (map[string]any, map[string]any, error) {
	defaults := make(map[string]any)

	for name, value := range g.inheritedDefaults(path) {
		defaults[name] = value
	}

	for name, value := range local {
		if err := checkDeclaration(name); err != nil {
			return nil, nil, fmt.Errorf("%s: vars: %w", path, err)
		}

		switch value.(type) {
		case bool, string:
		default:
			return nil, nil, fmt.Errorf("%s: vars: %q must be a boolean or string", path, name)
		}

		defaults[name] = value
	}

	resolved := make(map[string]any)

	for name, value := range g.vars {
		resolved[name] = value
	}

	for name, value := range defaults {
		resolved[name] = value
	}

	for name := range g.overrides {
		resolved[name] = g.vars[name]
	}

	return defaults, resolved, nil
}

func (g *graph) inherit(scope string, paths []string, defaults map[string]any) {
	for _, path := range paths {
		path = filepath.Clean(path)
		if _, ok := g.inherited[path]; !ok {
			g.inherited[path] = inheritance{filepath.Clean(scope), defaults}
		}
	}
}

func (g *graph) load(paths []string) ([]artifact, error) {
	var result []artifact
	for len(paths) > 0 {
		path := filepath.Clean(paths[0])
		paths = paths[1:]

		if g.ignores(path) {
			continue
		}

		if g.seen[path] {
			continue
		}

		if err := noSymlinks(g.root, path); err != nil {
			return nil, err
		}

		g.seen[path] = true
		m, sum, err := readManifest(path)
		if err != nil {
			return nil, err
		}

		g.digests[path] = sum

		defaults, vars, err := g.manifestVariables(path, m.Vars)
		if err != nil {
			return nil, err
		}

		g.loaded[path] = defaults

		for _, name := range m.Required {
			if strings.TrimSpace(name) == "" {
				return nil, fmt.Errorf("%s: required artifact name is empty", path)
			}

			g.required[name] = true
		}

		for _, a := range m.Artifacts {
			active, err := evaluate(a.Condition, vars)
			if err != nil {
				return nil, fmt.Errorf("%s: artifact %q condition: %w", path, a.Name, err)
			}

			if !active {
				continue
			}

			if err := g.add(&a, path); err != nil {
				return nil, err
			}

			a.defaults = defaults
			result = append(result, a)
			if a.IgnoreDepot {
				g.ignored = append(g.ignored, a.dest)
			}
		}

		for _, include := range m.Includes {
			dir, err := workspacePath(g.root, include)
			if err != nil {
				return nil, fmt.Errorf("%s: include: %w", path, err)
			}

			found, err := g.discover(dir)
			if err != nil {
				return nil, err
			}

			for _, candidate := range found {
				if g.ignores(candidate) {
					continue
				}

				pending := false
				for _, a := range result {
					if within(a.dest, candidate) {
						pending = true
						break
					}
				}

				// Read manifests shipped by this batch only after installing
				// their parent, never from a previous artifact version.
				if !pending {
					g.inherit(dir, []string{candidate}, defaults)
					paths = append(paths, candidate)
				}
			}
		}
	}

	return result, nil
}

func (g *graph) ignores(path string) bool {
	for _, dest := range g.ignored {
		if within(dest, path) {
			return true
		}
	}

	return false
}

func (g *graph) add(a *artifact, path string) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: artifact %q: %s", path, a.Name, fmt.Sprintf(format, args...))
	}

	if strings.TrimSpace(a.Name) == "" || a.To == "" {
		return fail("name and to are required")
	}

	// A missing checksum is allowed while validating manifests (configure),
	// but sync refuses to download anything until it is provided.
	if a.SHA256 != "" {
		sum, err := hex.DecodeString(a.SHA256)
		if err != nil || len(sum) != 32 {
			return fail("sha256 must contain exactly 64 hexadecimal characters")
		}

		a.SHA256 = strings.ToLower(a.SHA256)
	}

	u, err := url.Parse(a.From)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Fragment != "" {
		return fail("from must be an HTTP(S) URL without credentials or a fragment")
	}

	a.dest, err = workspacePath(g.root, a.To)
	if err != nil {
		return fail("%s", err)
	}

	if a.dest == g.root || within(a.dest, path) {
		return fail("destination would replace its own manifest")
	}

	format, err := archiveFormat(*a)
	if err != nil {
		return fail("%s", err)
	}

	if err := checkStripPrefix(*a, format); err != nil {
		return fail("%s", err)
	}

	if previous, ok := g.names[a.Name]; ok {
		return fail("multiple active artifacts share this name (also in %s)", previous)
	}

	for dest, name := range g.targets {
		if dest == a.dest {
			return fail("destination overlaps artifact %q at %s", name, dest)
		}

		if within(dest, a.dest) && !within(dest, path) {
			return fail("destination overlaps artifact %q outside its manifest subtree", name)
		}

		if within(a.dest, dest) && !within(a.dest, g.names[name]) {
			return fail("destination overlaps artifact %q outside its manifest subtree", name)
		}
	}

	g.names[a.Name] = path
	g.targets[a.dest] = a.Name
	return nil
}

// missingChecksums lists active artifacts that cannot be downloaded yet.
func missingChecksums(artifacts []artifact) []string {
	var missing []string
	for _, a := range artifacts {
		if a.SHA256 == "" {
			missing = append(missing, a.Name)
		}
	}

	sort.Strings(missing)
	return missing
}

func (g *graph) checkRequired() error {
	var missing []string
	for name := range g.required {
		if _, ok := g.names[name]; !ok {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("required artifacts have no active definition: %s", strings.Join(missing, ", "))
	}

	return nil
}

// discover searches dir and records the result as an input of the graph.
func (g *graph) discover(dir string) ([]string, error) {
	found, err := discover(dir)
	if err != nil {
		return nil, err
	}

	g.scans[filepath.Clean(dir)] = found
	return found, nil
}

func discover(dir string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) && path == dir {
			return nil
		}

		if err != nil {
			return err
		}

		if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == ".cache") {
			return filepath.SkipDir
		}

		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink during manifest discovery: %s", path)
		}

		if !entry.IsDir() && entry.Name() == manifestName {
			paths = append(paths, path)
		}

		return nil
	})

	// Shallower manifests are loaded first so they can pass their defaults on
	// to manifests in their subdirectories.
	sort.SliceStable(paths, func(i, j int) bool {
		return strings.Count(paths[i], string(filepath.Separator)) < strings.Count(paths[j], string(filepath.Separator))
	})

	return paths, err
}
