package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadableErrorLogging(t *testing.T) {
	var output bytes.Buffer
	cmd := newCommand()
	cmd.SetErr(&output)
	log := (&options{}).logger(cmd)
	log.Error("validation failed", "error", errors.Join(
		errors.New(`artifact "tool": invalid condition`),
		errors.New("expected closing parenthesis"),
	))

	text := output.String()
	if !strings.Contains(text, `artifact "tool": invalid condition`) ||
		!strings.Contains(text, "\n  expected closing parenthesis") ||
		strings.Contains(text, `\n`) {
		t.Fatalf("error output is escaped instead of readable:\n%s", text)
	}
}

func TestCacheStoresOnlyDownloads(t *testing.T) {
	root := t.TempDir()
	data := tarGzip(t, map[string][]byte{"pkg/bin/tool": []byte("tool")})
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(data)
	})
	writeManifest(t, root, stripPrefixManifest(server.URL, data, "pkg"))

	configure(t, root)

	if err := runCLI(t, root, "sync"); err != nil {
		t.Fatal(err)
	}

	object := filepath.Join(root, ".cache", "depot", "objects", digest(data))
	entries, err := os.ReadDir(object)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 || entries[0].Name() != "source" {
		t.Fatalf("cache retains extracted data: %v", entries)
	}

	ignore := filepath.Join(root, ".cache", "depot", ".gitignore")
	if got := readString(t, ignore); !strings.Contains(got, "*") {
		t.Fatalf("cache gitignore does not ignore contents: %q", got)
	}

	// A current artifact needs neither extraction nor the original download.
	server.Close()
	if err := os.Remove(filepath.Join(object, "source")); err != nil {
		t.Fatal(err)
	}

	if err := runCLI(t, root, "sync"); err != nil {
		t.Fatalf("unchanged artifact accessed its missing archive: %v", err)
	}
}

func TestIgnoreDepot(t *testing.T) {
	root := t.TempDir()
	data := tarGzip(t, map[string][]byte{
		"DEPOT.yaml":        []byte("not: a valid manifest\n"),
		"nested/DEPOT.yaml": []byte("also: invalid\n"),
		"bin/tool":          []byte("tool"),
	})
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(data)
	})

	// Includes must not rediscover ignored files on a subsequent invocation.
	content := "includes: [.]\nartifacts:\n" +
		"  - name: tool\n" +
		"    from: " + server.URL + "/tool.tar.gz\n" +
		"    to: third_party/tool\n" +
		"    sha256: " + digest(data) + "\n" +
		"    ignore_depot: true\n"
	writeFile(t, filepath.Join(root, manifestName), []byte(content))

	for _, command := range []string{"configure", "sync", "configure", "validate", "sync"} {
		if err := runCLI(t, root, command); err != nil {
			t.Fatalf("%s read ignored manifests: %v", command, err)
		}
	}

	if got := readString(t, filepath.Join(root, "third_party", "tool", "bin", "tool")); got != "tool" {
		t.Fatalf("artifact contents were not installed: %q", got)
	}

	// Turning discovery back on must take effect even for a current artifact.
	writeFile(t, filepath.Join(root, manifestName),
		[]byte(strings.ReplaceAll(content, "ignore_depot: true", "ignore_depot: false")))
	wantError(t, runCLI(t, root, "validate"), "field not not found")
	wantError(t, runCLI(t, root, "sync"), "field not not found")
}

func TestCacheGitignorePreservesExistingRules(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, manifest{})
	path := filepath.Join(root, ".cache", ".gitignore")
	writeFile(t, path, []byte("custom/\n"))

	if err := runCLI(t, root, "configure"); err != nil {
		t.Fatal(err)
	}

	if got := readString(t, path); got != "custom/\n" {
		t.Fatalf("existing ignore rules changed: %q", got)
	}

	if got := readString(t, filepath.Join(root, ".cache", "depot", ".gitignore")); got != cacheIgnore {
		t.Fatalf("depot cache is not ignored: %q", got)
	}

	// The unmodified file written to .cache/ by older versions is removed.
	writeFile(t, path, []byte(cacheIgnore))
	configure(t, root)

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy .cache/.gitignore was kept: %v", err)
	}
}

func TestCleanRetainsConfiguration(t *testing.T) {
	root := t.TempDir()
	leaf := []byte("dependency")
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(leaf)
	})
	child := manifest{
		Artifacts: []artifact{
			{
				Name:   "leaf",
				To:     "tools/nested/leaf.txt",
				From:   server.URL + "/leaf",
				SHA256: digest(leaf),
			},
		},
	}
	data := tarGzip(t, map[string][]byte{
		"DEPOT.yaml": manifestBytes(t, child),
		"bin/tool":   []byte("tool"),
	})
	bundleServer := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(data)
	})
	writeManifest(t, root, stripPrefixManifest(bundleServer.URL, data, ""))
	keep := filepath.Join(root, ".cache", "other-tool", "keep")
	writeFile(t, keep, []byte("unrelated"))

	if err := runCLI(t, root, "configure", "--set", "channel=nightly"); err != nil {
		t.Fatal(err)
	}

	if err := runCLI(t, root, "sync"); err != nil {
		t.Fatal(err)
	}

	statePath := filepath.Join(root, ".cache", "depot", "state.json")
	before := readString(t, statePath)
	for range 2 {
		if err := runCLI(t, root, "clean"); err != nil {
			t.Fatal(err)
		}
	}

	for _, path := range []string{"tools", ".cache/depot/objects"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("clean retained %s: %v", path, err)
		}
	}

	if after := readString(t, statePath); after != before {
		t.Fatal("clean modified saved state")
	}

	if got := readString(t, keep); got != "unrelated" {
		t.Fatal("clean changed an unrelated cache")
	}

	if err := runCLI(t, root, "sync"); err != nil {
		t.Fatalf("sync after clean: %v", err)
	}

	if got := readString(t, filepath.Join(root, "tools", "nested", "leaf.txt")); got != string(leaf) {
		t.Fatalf("sync did not restore transitive dependency: %q", got)
	}

	writeFile(t, filepath.Join(root, "tools", "nested", "leaf.txt"), []byte("local edit"))
	wantError(t, runCLI(t, root, "clean"), "locally modified")
	if got := readString(t, filepath.Join(root, "tools", "bin", "tool")); got != "tool" {
		t.Fatal("failed clean removed artifacts before completing safety checks")
	}
}

func TestSyncRetiresRemovedArtifacts(t *testing.T) {
	root := t.TempDir()
	data := []byte("old dependency")
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(data)
	})
	m := manifest{
		Artifacts: []artifact{
			{
				Name:   "old",
				To:     "third_party/old.txt",
				From:   server.URL + "/old",
				SHA256: digest(data),
			},
		},
	}
	writeManifest(t, root, m)

	configure(t, root)

	if err := runCLI(t, root, "sync"); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(root, "third_party", "old.txt")
	writeFile(t, filepath.Join(root, manifestName), []byte("invalid: manifest\n"))
	if err := runCLI(t, root, "sync"); err == nil {
		t.Fatal("invalid manifest unexpectedly synchronized")
	}

	if got := readString(t, dest); got != string(data) {
		t.Fatal("failed sync removed the old artifact")
	}

	writeManifest(t, root, manifest{})
	writeFile(t, dest, []byte("local edit"))
	wantError(t, runCLI(t, root, "sync"), "locally modified")
	writeFile(t, dest, data)

	if err := runCLI(t, root, "sync"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed dependency still installed: %v", err)
	}

	saved, err := readState(filepath.Join(root, ".cache", "depot", "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	if len(saved.Installed) != 0 {
		t.Fatalf("state contains removed dependencies: %+v", saved.Installed)
	}
}

// runCLIOutput is runCLI but returns the log output.
func runCLIOutput(t *testing.T, root string, args ...string) (string, error) {
	t.Helper()
	var output bytes.Buffer
	cmd := newCommand()
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs(append([]string{"--root", root}, args...))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := cmd.ExecuteContext(ctx)
	return output.String(), err
}

func TestSyncRequiresConfigure(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, manifest{})

	for _, command := range []string{"sync", "clean"} {
		wantError(t, runCLI(t, root, command), "depot configure")
	}

	if _, err := os.Stat(filepath.Join(root, ".cache")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unconfigured run created the cache: %v", err)
	}

	configure(t, root)
	if err := runCLI(t, root, "sync"); err != nil {
		t.Fatal(err)
	}
}

func TestStampDetectsManifestChanges(t *testing.T) {
	root := t.TempDir()
	data := []byte("dependency")
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(data)
	})
	child := manifest{
		Vars: map[string]any{"enabled": true},
		Artifacts: []artifact{
			{
				Name:      "leaf",
				To:        "third_party/leaf.txt",
				From:      server.URL + "/leaf",
				SHA256:    digest(data),
				Condition: "enabled",
			},
		},
	}
	writeManifest(t, root, manifest{Includes: []string{"build"}})
	writeManifest(t, filepath.Join(root, "build"), child)
	configure(t, root)

	sync := func(want string) {
		t.Helper()
		output, err := runCLIOutput(t, root, "sync")
		if err != nil {
			t.Fatal(err)
		}

		if !strings.Contains(output, want) {
			t.Fatalf("sync output does not contain %q:\n%s", want, output)
		}
	}

	sync("no completed sync recorded")
	sync("using state instead of evaluating")
	sync("workspace is up to date")

	// Line ending conversions, e.g. by git, are not changes.
	path := filepath.Join(root, "build", manifestName)
	lf := readString(t, path)
	for _, ending := range []string{"\r\n", "\r"} {
		writeFile(t, path, []byte(strings.ReplaceAll(lf, "\n", ending)))
		sync("workspace is up to date")
	}

	writeFile(t, filepath.Join(root, "build", "extra", manifestName), []byte("artifacts: []\r\n"))
	sync("manifests were added or removed in build")
	sync("workspace is up to date")

	configure(t, root, "--set", "enabled=false")
	sync("variables changed")
	if _, err := os.Stat(filepath.Join(root, "third_party", "leaf.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled artifact is still installed: %v", err)
	}

	configure(t, root, "--set", "enabled=true")
	sync("variables changed")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	sync(filepath.Join("build", manifestName) + " was removed")
	if _, err := os.Stat(filepath.Join(root, "third_party", "leaf.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact of a removed manifest is still installed: %v", err)
	}

	sync("workspace is up to date")
}
