package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ulikunitz/xz"
	"go.yaml.in/yaml/v3"
)

// zeroSHA256 is a syntactically valid checksum for artifacts that are never
// downloaded by a test.
var zeroSHA256 = strings.Repeat("0", 64)

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

func manifestBytes(t *testing.T, m manifest) []byte {
	t.Helper()
	data, err := yaml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	return data
}

// writeManifest writes m as the DEPOT.yaml file in dir.
func writeManifest(t *testing.T, dir string, m manifest) {
	t.Helper()
	writeFile(t, filepath.Join(dir, manifestName), manifestBytes(t, m))
}

// configure runs depot configure, which sync requires, and fails on error.
func configure(t *testing.T, root string, args ...string) {
	t.Helper()

	if err := runCLI(t, root, append([]string{"configure"}, args...)...); err != nil {
		t.Fatal(err)
	}
}

func runCLI(t *testing.T, root string, args ...string) error {
	t.Helper()
	cmd := newCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(append([]string{"--root", root}, args...))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return cmd.ExecuteContext(ctx)
}

// wantError fails the test unless err is non-nil and mentions want.
func wantError(t *testing.T, err error, want string) {
	t.Helper()

	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("got error %v, want error containing %q", err, want)
	}
}

// newTestServer starts an HTTP server that is closed when the test ends.
func newTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func tarGzip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)

	for name, data := range files {
		header := &tar.Header{
			Name: name,
			Mode: 0o755,
			Size: int64(len(data)),
		}

		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}

		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	return b.Bytes()
}

func TestRecursiveSyncAndOfflineCache(t *testing.T) {
	root := t.TempDir()

	var requests atomic.Int32
	files := map[string][]byte{}
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		data, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}

		w.Write(data)
	})

	leaf := []byte("verified dependency\n")
	child := manifest{
		Vars:     map[string]any{"leaf_enabled": true},
		Includes: []string{"."},
		Artifacts: []artifact{
			{
				Name:      "leaf",
				To:        "third_party/leaf.txt",
				From:      server.URL + "/leaf",
				SHA256:    digest(leaf),
				Condition: "(not use_old_unicode and (leaf_enabled or false)) and target_cpu == host_cpu",
			},
		},
		Required: []string{"leaf"},
	}

	bundle := tarGzip(t, map[string][]byte{
		"build/DEPOT.yaml": manifestBytes(t, child),
		"bin/tool":         []byte("tool contents"),
	})

	files["/bundle.tar.gz"] = bundle
	files["/leaf"] = leaf

	top := manifest{
		Vars: map[string]any{"use_old_unicode": false},
		Artifacts: []artifact{
			{
				Name:   "bundle",
				To:     "tools",
				From:   server.URL + "/bundle.tar.gz",
				SHA256: digest(bundle),
			},
		},
	}

	writeManifest(t, root, top)
	unrelatedCache := filepath.Join(root, ".cache", "clangd", "keep")
	writeFile(t, unrelatedCache, []byte("unrelated cache"))

	if err := runCLI(t, root, "configure"); err != nil {
		t.Fatal(err)
	}

	if requests.Load() != 0 {
		t.Fatal("configure performed a download")
	}

	if err := runCLI(t, root, "sync", "--jobs", "2"); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(root, "third_party", "leaf.txt")
	if got := readString(t, dest); got != string(leaf) {
		t.Fatalf("nested artifact = %q, want %q", got, leaf)
	}

	info, err := os.Stat(filepath.Join(root, "tools", "bin", "tool"))
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm()&0o111 == 0 {
		t.Fatal("executable permissions not preserved")
	}

	server.Close()

	if err := runCLI(t, root, "sync"); err != nil {
		t.Fatalf("offline sync: %v", err)
	}

	if err := os.RemoveAll(filepath.Join(root, "tools")); err != nil {
		t.Fatal(err)
	}

	if err := runCLI(t, root, "sync"); err != nil {
		t.Fatalf("restore from compressed cache: %v", err)
	}

	if got := readString(t, dest); got != string(leaf) {
		t.Fatalf("restored nested artifact = %q, want %q", got, leaf)
	}

	if _, err := os.Stat(unrelatedCache); err != nil {
		t.Fatal("unrelated cache was modified")
	}

	if requests.Load() != 2 {
		t.Fatalf("got %d downloads, want the two initial downloads only", requests.Load())
	}

	writeFile(t, dest, []byte("local modification"))
	wantError(t, runCLI(t, root, "sync"), "locally modified")
}

func TestValidationBeforeDownload(t *testing.T) {
	var requests atomic.Int32
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Write([]byte("data"))
	})

	valid := artifact{
		Name:   "one",
		To:     "third_party/one",
		From:   server.URL,
		SHA256: digest([]byte("data")),
	}

	tests := []struct {
		name string
		edit func(*manifest)
		want string
	}{
		{
			name: "missing checksum",
			edit: func(m *manifest) { m.Artifacts[0].SHA256 = "" },
			want: "sha256",
		},
		{
			name: "invalid checksum",
			edit: func(m *manifest) { m.Artifacts[0].SHA256 = strings.Repeat("z", 64) },
			want: "sha256",
		},
		{
			name: "duplicate name",
			edit: func(m *manifest) {
				a := valid
				a.To = "elsewhere"
				m.Artifacts = append(m.Artifacts, a)
			},
			want: "multiple active",
		},
		{
			name: "duplicate destination",
			edit: func(m *manifest) {
				a := valid
				a.Name = "two"
				m.Artifacts = append(m.Artifacts, a)
			},
			want: "overlaps",
		},
		{
			name: "required inactive",
			edit: func(m *manifest) {
				m.Artifacts[0].Condition = "false"
				m.Required = []string{"one"}
			},
			want: "required artifacts",
		},
		{
			name: "traversal",
			edit: func(m *manifest) { m.Artifacts[0].To = "../escape" },
			want: "relative",
		},
		{
			name: "self replacement",
			edit: func(m *manifest) { m.Artifacts[0].To = "." },
			want: "own manifest",
		},
		{
			name: "reserved path",
			edit: func(m *manifest) { m.Artifacts[0].To = ".cache/clangd/x" },
			want: "reserved",
		},
		{
			name: "undefined variable",
			edit: func(m *manifest) { m.Artifacts[0].Condition = "missing" },
			want: "undefined variable",
		},
		{
			name: "unsupported archive",
			edit: func(m *manifest) { m.Artifacts[0].Archive = "rar" },
			want: "unsupported archive",
		},
		{
			name: "unsupported URL",
			edit: func(m *manifest) { m.Artifacts[0].From = "file:///tmp/no" },
			want: "HTTP(S)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			m := manifest{Artifacts: []artifact{valid}}
			tt.edit(&m)
			writeManifest(t, root, manifest{})
			configure(t, root)
			writeManifest(t, root, m)

			wantError(t, runCLI(t, root, "sync"), tt.want)
		})
	}

	if requests.Load() != 0 {
		t.Fatalf("invalid manifests caused %d downloads", requests.Load())
	}
}

func TestIncludesAndStrictYAML(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, manifestName), []byte("includes: [build, absent]\nrequired: [included]\n"))
	nested := manifest{
		Includes: []string{"."},
		Artifacts: []artifact{
			{
				Name:   "included",
				To:     "file",
				From:   "https://example.test/data",
				SHA256: zeroSHA256,
			},
		},
	}

	writeManifest(t, filepath.Join(root, "build", "nested"), nested)

	if err := runCLI(t, root, "configure"); err != nil {
		t.Fatal(err)
	}

	invalid := []string{
		"artifcats: []\n",
		"artifacts: []\n---\nartifacts: []\n",
		"includes: []\nincludes: []\n",
		"",
	}

	for _, data := range invalid {
		writeFile(t, filepath.Join(root, manifestName), []byte(data))

		if err := runCLI(t, root, "configure"); err == nil {
			t.Errorf("accepted invalid YAML %q", data)
		}
	}
}

func TestChecksumFailureAndRecovery(t *testing.T) {
	root := t.TempDir()
	expected := []byte("correct")

	var bad atomic.Bool
	bad.Store(true)
	var requests atomic.Int32
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)

		if bad.Load() {
			w.Write([]byte("wrong"))
			return
		}

		w.Write(expected)
	})

	m := manifest{
		Artifacts: []artifact{
			{
				Name:   "file",
				To:     "file.txt",
				From:   server.URL,
				SHA256: digest(expected),
			},
		},
	}

	writeManifest(t, root, m)

	configure(t, root)

	wantError(t, runCLI(t, root, "sync"), "sha256 mismatch")
	dest := filepath.Join(root, "file.txt")
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("unverified artifact was installed")
	}

	bad.Store(false)

	if err := runCLI(t, root, "sync"); err != nil {
		t.Fatal(err)
	}

	cachedSource := filepath.Join(root, ".cache", "depot", "objects", digest(expected), "source")
	writeFile(t, cachedSource, []byte("cache corruption"))

	// A current destination does not need the cached source. Removing it
	// forces cache verification and recovery before the next installation.
	if err := os.Remove(dest); err != nil {
		t.Fatal(err)
	}

	if err := runCLI(t, root, "sync"); err != nil {
		t.Fatal(err)
	}

	if requests.Load() != 3 {
		t.Fatalf("got %d requests, want the corrupt source to be downloaded again (3)", requests.Load())
	}

	writeFile(t, dest, []byte("user edit"))
	wantError(t, runCLI(t, root, "sync"), "locally modified")
}

func TestConcurrentDownloads(t *testing.T) {
	root := t.TempDir()

	// Each request blocks until a second one arrives, so the sync can only
	// succeed if both downloads are in flight at the same time.
	gate := make(chan struct{})
	var once sync.Once
	var arrived atomic.Int32
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if arrived.Add(1) == 2 {
			once.Do(func() { close(gate) })
		}

		select {
		case <-gate:
			w.Write([]byte(r.URL.Path))
		case <-r.Context().Done():
		}
	})

	var m manifest
	for _, name := range []string{"a", "b"} {
		m.Artifacts = append(m.Artifacts, artifact{
			Name:   name,
			To:     name,
			From:   server.URL + "/" + name,
			SHA256: digest([]byte("/" + name)),
		})
	}

	writeManifest(t, root, m)

	configure(t, root)

	if err := runCLI(t, root, "sync", "--jobs", "2", "--timeout", "3s"); err != nil {
		t.Fatalf("downloads were not concurrent: %v", err)
	}
}

func TestUnsafeArchives(t *testing.T) {
	tests := []struct {
		name string
		kind byte
	}{
		{name: "../escape", kind: tar.TypeReg},
		{name: "/absolute", kind: tar.TypeReg},
		{name: `dir\escape`, kind: tar.TypeReg},
		{name: "link", kind: tar.TypeSymlink},
		{name: "hardlink", kind: tar.TypeLink},
		{name: "device", kind: tar.TypeChar},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()

			var data bytes.Buffer
			tw := tar.NewWriter(&data)
			header := &tar.Header{
				Name:     tt.name,
				Typeflag: tt.kind,
				Mode:     0o644,
				Linkname: "target",
			}

			if err := tw.WriteHeader(header); err != nil {
				t.Fatal(err)
			}

			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}

			source := filepath.Join(root, "source.tar")
			writeFile(t, source, data.Bytes())
			dest := filepath.Join(root, "out")
			if err := os.Mkdir(dest, 0o755); err != nil {
				t.Fatal(err)
			}

			if err := extract(context.Background(), source, dest, "tar"); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}

	t.Run("zip traversal", func(t *testing.T) {
		root := t.TempDir()

		var data bytes.Buffer
		zw := zip.NewWriter(&data)
		f, err := zw.Create("../escape")
		if err != nil {
			t.Fatal(err)
		}

		f.Write([]byte("bad"))

		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}

		source := filepath.Join(root, "source.zip")
		writeFile(t, source, data.Bytes())

		if err := extract(context.Background(), source, filepath.Join(root, "out"), "zip"); err == nil {
			t.Fatal("unsafe zip accepted")
		}
	})

	t.Run("expansion bound", func(t *testing.T) {
		u := unpacker{
			ctx:       context.Background(),
			root:      t.TempDir(),
			remaining: 3,
		}

		if err := u.entry("large", 0o644, strings.NewReader("four"), false); err == nil {
			t.Fatal("expansion limit was ignored")
		}
	})
}

func TestArchiveHintsAndArchiveDestinations(t *testing.T) {
	for _, preserve := range []bool{false, true} {
		t.Run(fmt.Sprint(preserve), func(t *testing.T) {
			root := t.TempDir()
			data := tarGzip(t, map[string][]byte{"file.txt": []byte("payload")})
			server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Write(data)
			})

			dest := "unpacked"
			if preserve {
				dest = "kept.tar.gz"
			}

			m := manifest{
				Artifacts: []artifact{
					{
						Name:    "bundle",
						To:      dest,
						From:    server.URL + "/no-extension",
						Archive: "tar.gz",
						SHA256:  digest(data),
					},
				},
			}

			writeManifest(t, root, m)

			configure(t, root)

			if err := runCLI(t, root, "sync"); err != nil {
				t.Fatal(err)
			}

			want := "payload"
			if preserve {
				want = string(data)
			} else {
				dest = filepath.Join(dest, "file.txt")
			}

			if got := readString(t, filepath.Join(root, dest)); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

func TestUnmanagedAndSymlinkProtection(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(fmt.Sprint(symlink), func(t *testing.T) {
			root := t.TempDir()
			m := manifest{
				Artifacts: []artifact{
					{
						Name:   "a",
						To:     "existing",
						From:   "https://example.test/a",
						SHA256: zeroSHA256,
					},
				},
			}

			writeManifest(t, root, m)
			configure(t, root)

			existing := filepath.Join(root, "existing")

			if symlink {
				if err := os.Symlink(t.TempDir(), existing); err != nil {
					t.Skip(err)
				}
			} else {
				writeFile(t, existing, []byte("user data"))
			}

			err := runCLI(t, root, "sync")
			if err == nil {
				t.Fatal("sync replaced an unmanaged destination")
			}

			if !strings.Contains(err.Error(), "unmanaged") && !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("got error %v, want destination protection", err)
			}
		})
	}
}

func TestRepositoryManifest(t *testing.T) {
	m, _, err := readManifest(manifestName)
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("no example DEPOT.yaml in the repository")
	}

	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	writeManifest(t, root, m)

	if err := runCLI(t, root, "configure"); err != nil {
		t.Fatalf("the supplied manifest must be configurable: %v", err)
	}

	wantError(t, runCLI(t, root, "sync"), "without a sha256")

	if _, err := os.Stat(filepath.Join(root, "third_party")); !os.IsNotExist(err) {
		t.Fatalf("sync without checksums modified the workspace: %v", err)
	}

	// The supplied example is a schema/spec fixture, not a trusted set of
	// pins. Substitute test-only checksums without contacting its URLs.
	for i := range m.Artifacts {
		m.Artifacts[i].SHA256 = zeroSHA256
	}

	writeManifest(t, root, m)

	odinArch := map[string]string{
		"x64":   "amd64",
		"arm64": "arm64",
	}

	for _, old := range []bool{false, true} {
		for cpu, arch := range odinArch {
			vars := map[string]any{
				"host_cpu":        cpu,
				"use_old_unicode": old,
			}

			overrides := map[string]string{"use_old_unicode": fmt.Sprint(old)}
			g := newGraph(root, vars, overrides)

			batch, err := g.load([]string{filepath.Join(root, manifestName)})
			if err != nil {
				t.Fatal(err)
			}

			if err := g.checkRequired(); err != nil {
				t.Fatal(err)
			}

			if len(batch) != 2 || !strings.Contains(batch[0].From, arch) {
				t.Fatalf("incorrect architecture selection for %s: %+v", cpu, batch)
			}

			if strings.Contains(batch[1].From, "/17.0.0/") != old {
				t.Fatalf("incorrect Unicode selection: %+v", batch[1])
			}
		}
	}
}

func TestRemoveLegacyExtractedCache(t *testing.T) {
	root := t.TempDir()
	data := tarGzip(t, map[string][]byte{"file.txt": []byte("original")})

	var requests atomic.Int32
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Write(data)
	})

	m := manifest{
		Artifacts: []artifact{
			{
				Name:   "bundle",
				To:     "bundle",
				From:   server.URL + "/file.tar.gz",
				SHA256: digest(data),
			},
		},
	}

	writeManifest(t, root, m)

	configure(t, root)

	if err := runCLI(t, root, "sync"); err != nil {
		t.Fatal(err)
	}

	server.Close()
	tree := filepath.Join(root, ".cache", "depot", "objects", digest(data), "unpacked-tar.gz")
	writeFile(t, filepath.Join(tree, "file.txt"), []byte("corruption"))

	// Depot versions that extracted into the cache wrote no stamp.
	statePath := filepath.Join(root, ".cache", "depot", "state.json")
	saved, err := readState(statePath)
	if err != nil {
		t.Fatal(err)
	}

	saved.Stamp = nil
	if err := atomicJSON(statePath, saved); err != nil {
		t.Fatal(err)
	}

	if err := runCLI(t, root, "sync"); err != nil {
		t.Fatalf("could not remove legacy extracted cache: %v", err)
	}

	if _, err := os.Stat(tree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy extracted entry was not removed: %v", err)
	}

	if requests.Load() != 1 {
		t.Fatalf("unnecessary redownloads: %d", requests.Load())
	}
}

// archiveBytes returns payload stored as file.txt in format, or as the raw
// stream for the single-file formats gzip and xz.
func archiveBytes(t *testing.T, format string, payload []byte) []byte {
	t.Helper()
	var data bytes.Buffer

	writeTar := func(w io.Writer, name string) {
		tw := tar.NewWriter(w)
		header := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(payload)),
		}

		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}

		tw.Write(payload)

		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
	}

	switch format {
	case "tar":
		writeTar(&data, "./file.txt")
	case "zip":
		zw := zip.NewWriter(&data)
		f, err := zw.Create("file.txt")
		if err != nil {
			t.Fatal(err)
		}

		f.Write(payload)

		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	case "gzip":
		gz := gzip.NewWriter(&data)
		gz.Write(payload)

		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	case "xz", "tar.xz":
		xw, err := xz.NewWriter(&data)
		if err != nil {
			t.Fatal(err)
		}

		if format == "tar.xz" {
			writeTar(xw, "file.txt")
		} else {
			xw.Write(payload)
		}

		if err := xw.Close(); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown test archive format %q", format)
	}

	return data.Bytes()
}

func TestArchiveFormats(t *testing.T) {
	for _, format := range []string{"tar", "zip", "gzip", "xz", "tar.xz"} {
		t.Run(format, func(t *testing.T) {
			payload := "uncompressed payload"
			root := t.TempDir()
			source := filepath.Join(root, "source")
			dest := filepath.Join(root, "extracted")
			writeFile(t, source, archiveBytes(t, format, []byte(payload)))

			if err := os.Mkdir(dest, 0o755); err != nil {
				t.Fatal(err)
			}

			if err := extract(context.Background(), source, dest, format); err != nil {
				t.Fatal(err)
			}

			file := "file.txt"
			if format == "gzip" || format == "xz" {
				file = "payload"
			}

			if got := readString(t, filepath.Join(dest, file)); got != payload {
				t.Fatalf("got decompressed output %q, want %q", got, payload)
			}
		})
	}
}

func TestHTTPFailuresDoNotReplaceInstalledArtifacts(t *testing.T) {
	for _, failure := range []string{"status", "truncated", "size", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()

			var fail atomic.Bool
			server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				if !fail.Load() {
					w.Write([]byte("original"))
					return
				}

				switch failure {
				case "status":
					w.WriteHeader(http.StatusServiceUnavailable)
				case "truncated":
					w.Header().Set("Content-Length", "100")
					w.Write([]byte("short"))
				case "size":
					w.Header().Set("Content-Length", fmt.Sprint(maxDownloadBytes+1))
				case "timeout":
					<-r.Context().Done()
				}
			})

			m := manifest{
				Artifacts: []artifact{
					{
						Name:   "a",
						To:     "installed",
						From:   server.URL,
						SHA256: digest([]byte("original")),
					},
				},
			}

			writeManifest(t, root, m)

			configure(t, root)

			if err := runCLI(t, root, "sync"); err != nil {
				t.Fatal(err)
			}

			fail.Store(true)
			m.Artifacts[0].SHA256 = digest([]byte("update"))
			writeManifest(t, root, m)

			if err := runCLI(t, root, "sync", "--timeout", "100ms"); err == nil {
				t.Fatal("broken download unexpectedly succeeded")
			}

			if got := readString(t, filepath.Join(root, "installed")); got != "original" {
				t.Fatalf("failed download replaced installed data: got %q", got)
			}
		})
	}
}

func TestSavedVariablesAndInvalidAssignments(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, manifestName), []byte("artifacts: []\n"))

	if err := runCLI(t, root, "configure", "--set", "flag=true", "--set", "label=a=b"); err != nil {
		t.Fatal(err)
	}

	if err := runCLI(t, root, "configure", "--set", "flag=false"); err != nil {
		t.Fatal(err)
	}

	statePath := filepath.Join(root, ".cache", "depot", "state.json")
	saved, err := readState(statePath)
	if err != nil {
		t.Fatal(err)
	}

	if saved.Variables["flag"] != "false" || saved.Variables["label"] != "a=b" {
		t.Fatalf("variables not retained: %+v", saved.Variables)
	}

	for _, assignment := range []string{"host_cpu=arm64", "missing-equals", "=value", "and=true"} {
		if err := runCLI(t, root, "configure", "--set", assignment); err == nil {
			t.Errorf("accepted assignment %q", assignment)
		}
	}

	saved, err = readState(statePath)
	if err != nil {
		t.Fatal(err)
	}

	if len(saved.Variables) != 2 {
		t.Fatalf("invalid configure modified saved state: %+v", saved.Variables)
	}
}

func TestRootRelativeIncludesAndVariableDefaults(t *testing.T) {
	root := t.TempDir()
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.URL.Path))
	})

	// variants returns two definitions of name; exactly one is active
	// depending on the enabled variable, and only when flavor matches.
	variants := func(name, flavor string) []artifact {
		var artifacts []artifact
		for _, enabled := range []bool{false, true} {
			path := fmt.Sprintf("/%s/%t", name, enabled)
			condition := fmt.Sprintf(
				"(enabled == %t) and (flavor == '%s' and target_cpu == host_cpu)",
				enabled, flavor)
			artifacts = append(artifacts, artifact{
				Name:      name,
				To:        "third_party/" + name + ".txt",
				From:      server.URL + path,
				SHA256:    digest([]byte(path)),
				Condition: condition,
			})
		}

		return artifacts
	}

	top := manifest{
		Vars: map[string]any{
			"enabled": true,
			"flavor":  "root",
		},
		Includes: []string{"manifests/build", "manifests/sibling"},
	}

	child := manifest{
		Vars: map[string]any{
			"enabled": false,
			"flavor":  "child",
		},
		Includes:  []string{"manifests/deeper"},
		Artifacts: variants("child", "child"),
		Required:  []string{"child"},
	}

	deeper := manifest{
		Artifacts: variants("deeper", "child"),
		Required:  []string{"deeper"},
	}

	sibling := manifest{
		Artifacts: variants("sibling", "root"),
		Required:  []string{"sibling"},
	}

	writeManifest(t, root, top)
	writeManifest(t, filepath.Join(root, "manifests", "build"), child)
	writeManifest(t, filepath.Join(root, "manifests", "deeper"), deeper)
	writeManifest(t, filepath.Join(root, "manifests", "sibling"), sibling)

	assertInstalled := func(siblingEnabled, nestedEnabled bool) {
		t.Helper()

		if err := runCLI(t, root, "sync"); err != nil {
			t.Fatal(err)
		}

		want := map[string]bool{
			"child":   nestedEnabled,
			"deeper":  nestedEnabled,
			"sibling": siblingEnabled,
		}

		for name, enabled := range want {
			got := readString(t, filepath.Join(root, "third_party", name+".txt"))
			if want := fmt.Sprintf("/%s/%t", name, enabled); got != want {
				t.Fatalf("%s: got %q, want %q", name, got, want)
			}
		}

		nestedThirdParty := filepath.Join(root, "manifests", "build", "third_party")
		if _, err := os.Stat(nestedThirdParty); !os.IsNotExist(err) {
			t.Fatalf("nested third_party directory was created: %v", err)
		}
	}

	if err := runCLI(t, root, "configure"); err != nil {
		t.Fatal(err)
	}

	assertInstalled(true, false)

	top.Vars["enabled"] = false
	writeManifest(t, root, top)
	assertInstalled(false, false)

	if err := runCLI(t, root, "configure", "--set", "enabled=true"); err != nil {
		t.Fatal(err)
	}

	assertInstalled(true, true)

	saved, err := readState(filepath.Join(root, ".cache", "depot", "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	if len(saved.Variables) != 1 || saved.Variables["enabled"] != "true" {
		t.Fatalf("only explicit overrides should be persisted: %v", saved.Variables)
	}
}

func TestInvalidManifestDefaults(t *testing.T) {
	tests := []struct {
		name string
		vars string
		want string
	}{
		{name: "host cpu", vars: "host_cpu: x64", want: "read-only"},
		{name: "host os", vars: "host_os: linux", want: "read-only"},
		{name: "host prefix", vars: "host_abi: gnu", want: "reserved"},
		{name: "target cpu", vars: "target_cpu: arm64", want: "reserved"},
		{name: "target os", vars: "target_os: windows", want: "reserved"},
		{name: "target prefix", vars: "target_abi: gnu", want: "reserved"},
		{name: "operator", vars: "and: true", want: "invalid variable name"},
		{name: "invalid name", vars: "bad-name: false", want: "invalid variable name"},
		{name: "number", vars: "version: 17", want: "boolean or string"},
		{name: "null", vars: "enabled: null", want: "boolean or string"},
		{name: "list", vars: "enabled: [true]", want: "boolean or string"},
		{name: "map", vars: "enabled: {default: true}", want: "boolean or string"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, manifestName), []byte("vars:\n  "+tt.vars+"\n"))

			wantError(t, runCLI(t, root, "configure"), tt.want)
		})
	}
}

func TestExplicitOverridesBeatStringAndTargetDefaults(t *testing.T) {
	root := t.TempDir()
	m := manifest{
		Vars: map[string]any{
			"version": "17",
			"enabled": true,
		},
		Artifacts: []artifact{
			{
				Name:      "selected",
				To:        "third_party/selected",
				From:      "https://example.test/selected",
				Condition: "target_cpu == 'override-cpu' and (version == '18' and not enabled)",
			},
		},
		Required: []string{"selected"},
	}

	writeManifest(t, root, m)

	err := runCLI(t, root, "configure",
		"--set", "target_cpu=override-cpu",
		"--set", "version=18",
		"--set", "enabled=false",
	)

	if err != nil {
		t.Fatal(err)
	}

	if err := runCLI(t, root, "configure"); err != nil {
		t.Fatalf("saved overrides did not beat manifest defaults: %v", err)
	}
}

func TestParentUpdateUsesNewNestedManifest(t *testing.T) {
	root := t.TempDir()

	var version atomic.Int32
	version.Store(1)
	var bundles [2][]byte
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bundle.tar.gz" {
			w.Write(bundles[version.Load()-1])
			return
		}

		w.Write([]byte(r.URL.Path))
	})

	for i := range bundles {
		leafPath := fmt.Sprintf("/leaf%d", i+1)
		nested := manifest{
			Artifacts: []artifact{
				{
					Name:   "leaf",
					To:     "deps/leaf",
					From:   server.URL + leafPath,
					SHA256: digest([]byte(leafPath)),
				},
			},
		}

		bundles[i] = tarGzip(t, map[string][]byte{
			manifestName: manifestBytes(t, nested),
		})
	}

	for i := range bundles {
		version.Store(int32(i + 1))
		top := manifest{
			Includes: []string{"tools"},
			Artifacts: []artifact{
				{
					Name:   "tools",
					To:     "tools",
					From:   server.URL + "/bundle.tar.gz",
					SHA256: digest(bundles[i]),
				},
			},
		}

		writeManifest(t, root, top)

		configure(t, root)

		if err := runCLI(t, root, "sync"); err != nil {
			t.Fatal(err)
		}

		got := readString(t, filepath.Join(root, "deps", "leaf"))
		if want := fmt.Sprintf("/leaf%d", i+1); got != want {
			t.Fatalf("nested manifest was stale after parent update: got %q, want %q", got, want)
		}
	}
}

func TestInstallationRollsBackOnStateWriteFailure(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "installed")
	source := filepath.Join(root, "source")
	writeFile(t, dest, []byte("previous"))
	writeFile(t, source, []byte("replacement"))

	sum, err := treeChecksum(context.Background(), dest)
	if err != nil {
		t.Fatal(err)
	}

	old := installation{
		SHA256: digest([]byte("previous")),
		Tree:   sum,
	}

	s := &state{
		Variables: map[string]string{},
		Installed: map[string]installation{"installed": old},
	}

	// A directory cannot be replaced by the atomic state write, forcing the
	// commit to fail after the new payload has been moved into place.
	badStatePath := filepath.Join(root, "state-is-a-directory")
	if err := os.Mkdir(badStatePath, 0o755); err != nil {
		t.Fatal(err)
	}

	e := &engine{
		root:      root,
		statePath: badStatePath,
		state:     s,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	a := artifact{
		Name:   "a",
		SHA256: digest([]byte("replacement")),
		dest:   dest,
	}

	if err := e.install(context.Background(), a, source); err == nil {
		t.Fatal("expected state commit failure")
	}

	if got := readString(t, dest); got != "previous" {
		t.Fatalf("previous destination was not restored: got %q", got)
	}

	if s.Installed["installed"] != old {
		t.Fatal("in-memory installation state was not restored")
	}

	stages, err := filepath.Glob(filepath.Join(root, ".depot-install-*"))
	if err != nil {
		t.Fatal(err)
	}

	if len(stages) != 0 {
		t.Fatalf("unexpected leftover staging paths: %v", stages)
	}
}

func TestVariableScoping(t *testing.T) {
	root := t.TempDir()
	child := manifest{
		Vars: map[string]any{"child_only": true},
		Artifacts: []artifact{
			{
				Name:      "child",
				To:        "third_party/child",
				From:      "https://example.test/child",
				SHA256:    zeroSHA256,
				Condition: "child_only and root_var == 'root'",
			},
		},
	}

	writeManifest(t, filepath.Join(root, "build"), child)

	tests := []struct {
		name      string
		condition string
		sibling   string
		want      string
	}{
		{
			name: "child inherits includer vars",
		},
		{
			name:      "includer cannot see child vars",
			condition: "child_only",
			want:      "undefined variable",
		},
		{
			name:    "sibling cannot see child vars",
			sibling: "child_only",
			want:    "undefined variable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			top := manifest{
				Vars:     map[string]any{"root_var": "root"},
				Includes: []string{"build", "sibling"},
			}

			if tt.condition != "" {
				top.Artifacts = []artifact{
					{
						Name:      "top",
						To:        "third_party/top",
						From:      "https://example.test/top",
						SHA256:    zeroSHA256,
						Condition: tt.condition,
					},
				}
			}

			sibling := manifest{}

			if tt.sibling != "" {
				sibling.Artifacts = []artifact{
					{
						Name:      "sibling",
						To:        "third_party/sibling",
						From:      "https://example.test/sibling",
						SHA256:    zeroSHA256,
						Condition: tt.sibling,
					},
				}
			}

			writeManifest(t, root, top)
			writeManifest(t, filepath.Join(root, "sibling"), sibling)

			err := runCLI(t, root, "validate")

			if tt.want == "" {
				// The artifact is not cached, so validation passes with a
				// planned download.
				if err != nil {
					t.Fatal(err)
				}

				return
			}

			wantError(t, err, tt.want)
		})
	}
}

func TestValidateIsReadOnlyDryRun(t *testing.T) {
	root := t.TempDir()
	payload := []byte("payload")

	var requests atomic.Int32
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Write(payload)
	})

	m := manifest{
		Artifacts: []artifact{
			{
				Name:      "a",
				To:        "third_party/a.txt",
				From:      server.URL,
				SHA256:    digest(payload),
				Condition: "enabled",
			},
		},
		Vars: map[string]any{"enabled": true},
	}

	writeManifest(t, root, m)

	if err := runCLI(t, root, "validate", "--set", "enabled=false"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(root, ".cache")); !os.IsNotExist(err) {
		t.Fatalf("validate created the cache directory: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "third_party")); !os.IsNotExist(err) {
		t.Fatalf("validate installed an artifact: %v", err)
	}

	if requests.Load() != 0 {
		t.Fatalf("validate performed %d downloads", requests.Load())
	}

	configure(t, root)

	if err := runCLI(t, root, "sync"); err != nil {
		t.Fatalf("validate --set must not be persisted: %v", err)
	}

	if err := runCLI(t, root, "validate"); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(root, "third_party", "a.txt"), []byte("local edit"))
	wantError(t, runCLI(t, root, "validate"), "locally modified")

	if got := readString(t, filepath.Join(root, "third_party", "a.txt")); got != "local edit" {
		t.Fatalf("validate changed a modified artifact: got %q", got)
	}

	if requests.Load() != 1 {
		t.Fatalf("got %d downloads, want only the one from sync", requests.Load())
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	root := t.TempDir()
	m := manifest{
		Artifacts: []artifact{
			{
				Name: "unpinned",
				To:   "third_party/unpinned",
				From: "https://example.test/unpinned",
			},
			{
				Name:   "unmanaged",
				To:     "third_party/unmanaged",
				From:   "https://example.test/unmanaged",
				SHA256: zeroSHA256,
			},
		},
	}

	writeManifest(t, root, m)
	writeFile(t, filepath.Join(root, "third_party", "unmanaged"), []byte("user data"))

	err := runCLI(t, root, "validate")
	wantError(t, err, "2 problem(s)")
	wantError(t, err, "no sha256")
	wantError(t, err, "unmanaged destination")
}

func TestSyncCreatesMissingParentDirectories(t *testing.T) {
	root := t.TempDir()
	leaf := []byte("leaf")
	files := map[string][]byte{"/leaf": leaf}
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(files[r.URL.Path])
	})

	// The parent's dependency installs into a deep directory that nobody
	// prepared, mirroring a transitive dependency the workspace doesn't know.
	nested := manifest{
		Artifacts: []artifact{
			{
				Name:   "leaf",
				To:     "third_party/deep/er/still/leaf.txt",
				From:   server.URL + "/leaf",
				SHA256: digest(leaf),
			},
		},
	}

	bundle := tarGzip(t, map[string][]byte{manifestName: manifestBytes(t, nested)})
	files["/bundle.tar.gz"] = bundle

	top := manifest{
		Artifacts: []artifact{
			{
				Name:   "bundle",
				To:     "vendor/tools/bundle",
				From:   server.URL + "/bundle.tar.gz",
				SHA256: digest(bundle),
			},
		},
	}

	writeManifest(t, root, top)

	configure(t, root)

	if err := runCLI(t, root, "sync"); err != nil {
		t.Fatal(err)
	}

	got := readString(t, filepath.Join(root, "third_party", "deep", "er", "still", "leaf.txt"))
	if got != string(leaf) {
		t.Fatalf("got %q, want %q", got, leaf)
	}
}

// stripPrefixManifest declares a single archive installed at tools.
func stripPrefixManifest(url string, data []byte, prefix string) manifest {
	return manifest{
		Artifacts: []artifact{
			{
				Name:        "bundle",
				To:          "tools",
				From:        url + "/bundle.tar.gz",
				SHA256:      digest(data),
				StripPrefix: prefix,
			},
		},
	}
}

func TestStripPrefix(t *testing.T) {
	root := t.TempDir()
	data := tarGzip(t, map[string][]byte{
		"pkg-1.0/inner/bin/tool":  []byte("tool"),
		"pkg-1.0/inner/README.md": []byte("readme"),
		"pkg-1.0/unwanted.txt":    []byte("unwanted"),
	})

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(data)
	})

	tools := filepath.Join(root, "tools")

	// A doubly nested directory becomes the destination root.
	writeManifest(t, root, stripPrefixManifest(server.URL, data, "pkg-1.0/inner"))

	configure(t, root)

	if err := runCLI(t, root, "sync"); err != nil {
		t.Fatal(err)
	}

	if got := readString(t, filepath.Join(tools, "bin", "tool")); got != "tool" {
		t.Fatalf("tools/bin/tool = %q, want %q", got, "tool")
	}

	if _, err := os.Stat(filepath.Join(tools, "unwanted.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("content outside strip_prefix was installed: %v", err)
	}

	// Changing only strip_prefix reinstalls; a prefix may select one file.
	writeManifest(t, root, stripPrefixManifest(server.URL, data, "pkg-1.0/inner/bin/tool/"))

	if err := runCLI(t, root, "sync"); err != nil {
		t.Fatal(err)
	}

	if got := readString(t, tools); got != "tool" {
		t.Fatalf("tools = %q, want %q", got, "tool")
	}

	writeManifest(t, root, stripPrefixManifest(server.URL, data, "pkg-2.0"))
	err := runCLI(t, root, "sync")
	wantError(t, err, `strip_prefix "pkg-2.0" not found in archive; top-level entries: pkg-1.0/`)

	if got := readString(t, tools); got != "tool" {
		t.Fatalf("failed sync changed the installed artifact: %q", got)
	}

	// Validation never extracts archives; sync checks that the prefix exists.
	if err := runCLI(t, root, "validate"); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidStripPrefix(t *testing.T) {
	tests := []struct {
		name   string
		to     string
		from   string
		prefix string
		want   string
	}{
		{
			name:   "parent",
			to:     "tools",
			from:   "/bundle.tar.gz",
			prefix: "../outside",
			want:   "must be a relative path inside the archive",
		},
		{
			name:   "absolute",
			to:     "tools",
			from:   "/bundle.zip",
			prefix: "/pkg",
			want:   "must be a relative path inside the archive",
		},
		{
			name:   "root",
			to:     "tools",
			from:   "/bundle.tar.xz",
			prefix: "./",
			want:   "must be a relative path inside the archive",
		},
		{
			name:   "plain file",
			to:     "video.mp4",
			from:   "/video.mp4",
			prefix: "pkg",
			want:   "strip_prefix requires a tar or zip archive",
		},
		{
			name:   "single file compression",
			to:     "data.txt",
			from:   "/data.txt.gz",
			prefix: "pkg",
			want:   "strip_prefix requires a tar or zip archive",
		},
		{
			name:   "archive kept as is",
			to:     "kept.tar.gz",
			from:   "/bundle.tar.gz",
			prefix: "pkg",
			want:   "strip_prefix requires a tar or zip archive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			m := manifest{
				Artifacts: []artifact{
					{
						Name:        "bundle",
						To:          tt.to,
						From:        "https://example.com" + tt.from,
						SHA256:      zeroSHA256,
						StripPrefix: tt.prefix,
					},
				},
			}

			writeManifest(t, root, m)
			wantError(t, runCLI(t, root, "configure"), tt.want)
		})
	}
}
