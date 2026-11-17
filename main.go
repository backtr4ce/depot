package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"
)

const (
	minJobs = 1
	maxJobs = 256

	maxRedirects = 10

	cacheDirectoryName    = ".cache"
	gitDirectoryName      = ".git"
	depotDirectoryName    = "depot"
	objectsDirectoryName  = "objects"
	stateFileName         = "state.json"
	workspaceLockFileName = "workspace.lock"
	cacheIgnoreFileName   = ".gitignore"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newCommand().ExecuteContext(ctx); err != nil {
		newLogger(os.Stderr, slog.LevelInfo).Error("depot failed", "error", err)
		os.Exit(1)
	}
}

// options holds the command line flags shared by all subcommands.
type options struct {
	root        string
	verbose     bool
	jobs        int
	timeout     time.Duration
	assignments []string
}

func (o *options) validate() error {
	if o.jobs < minJobs || o.jobs > maxJobs {
		return fmt.Errorf("jobs must be between %d and %d", minJobs, maxJobs)
	}

	if o.timeout <= 0 {
		return fmt.Errorf("timeout must be positive")
	}

	return nil
}

func (o *options) logger(cmd *cobra.Command) *slog.Logger {
	level := slog.LevelInfo
	if o.verbose {
		level = slog.LevelDebug
	}

	return newLogger(cmd.ErrOrStderr(), level)
}

func newCommand() *cobra.Command {
	opts := &options{}

	command := &cobra.Command{
		Use:           "depot",
		Short:         "Synchronize checksum-pinned workspace artifacts",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return errors.Join(cmd.Help(), fmt.Errorf("choose configure, validate, sync or clean"))
		},
	}

	flags := command.PersistentFlags()
	flags.StringVar(&opts.root, "root", ".", "Workspace directory containing DEPOT.yaml")
	flags.BoolVarP(&opts.verbose, "verbose", "v", false, "Enable debug logging")
	flags.IntVarP(&opts.jobs, "jobs", "j", min(runtime.NumCPU(), 8), "Maximum concurrent artifact downloads")
	flags.DurationVar(&opts.timeout, "timeout", 5*time.Minute, "Timeout per HTTP download")

	configure := &cobra.Command{
		Use:   "configure",
		Short: "Validate manifests and persist condition variables (no downloads)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, opts, configureMode, runConfigure)
		},
	}

	configure.Flags().StringArrayVar(
		&opts.assignments,
		"set",
		nil,
		"Set NAME=VALUE; repeat for multiple variables (true/false are booleans)",
	)

	validate := &cobra.Command{
		Use:   "validate",
		Short: "Dry run of sync: check manifests, checksums, destinations and cache without writing anything",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, opts, readMode, runValidate)
		},
	}

	validate.Flags().StringArrayVar(
		&opts.assignments,
		"set",
		nil,
		"Override NAME=VALUE for this run only (not saved); repeat for multiple variables",
	)

	sync := &cobra.Command{
		Use:   "sync",
		Short: "Download, verify and install active artifacts recursively",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, opts, writeMode, runSync)
		},
	}

	clean := &cobra.Command{
		Use:   "clean",
		Short: "Remove managed artifacts and cached downloads, retaining saved state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, opts, writeMode, runClean)
		},
	}

	command.AddCommand(configure, validate, sync, clean)
	return command
}

// session is the locked workspace a subcommand operates on.
type session struct {
	root      string
	cacheDir  string
	statePath string
	state     *state
	vars      map[string]any
	opts      *options
	log       *slog.Logger
}

// mode describes how a subcommand may use the workspace.
type mode int

const (
	// readMode never creates or writes anything and takes no lock.
	readMode mode = iota
	// configureMode creates the workspace state.
	configureMode
	// writeMode changes a workspace that configure has already created.
	writeMode
)

var errNotConfigured = errors.New("workspace is not configured; run depot configure first")

// run opens and locks the workspace, applies --set assignments and then
// invokes action while holding the workspace lock.
func run(
	cmd *cobra.Command,
	opts *options,
	m mode,
	action func(context.Context, *session) error,
) error {
	if err := opts.validate(); err != nil {
		return err
	}

	s := &session{opts: opts, log: opts.logger(cmd)}
	if err := s.openWorkspace(false); err != nil {
		return err
	}

	// Checked before anything is created and again while locked, in case a
	// concurrent run removed the state in between.
	if m == writeMode {
		if err := s.checkConfigured(); err != nil {
			return err
		}
	}

	if m != readMode {
		if err := s.openWorkspace(true); err != nil {
			return err
		}

		l, err := lock(cmd.Context(), filepath.Join(s.cacheDir, workspaceLockFileName))
		if err != nil {
			return err
		}

		defer unlock(l, s.log)

		if err := s.removeLegacyLocks(); err != nil {
			return err
		}
	}

	if m == writeMode {
		if err := s.checkConfigured(); err != nil {
			return err
		}
	}

	if err := s.loadVariables(); err != nil {
		return err
	}

	return action(cmd.Context(), s)
}

func (s *session) checkConfigured() error {
	_, err := os.Lstat(s.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return errNotConfigured
	}

	return err
}

// removeLegacyLocks deletes the per-download lock files of older depot
// versions. The caller holds the workspace lock, which they also took.
func (s *session) removeLegacyLocks() error {
	dir := filepath.Join(s.cacheDir, "locks")
	if err := noSymlinks(s.root, dir); err != nil {
		return err
	}

	return os.RemoveAll(dir)
}

func unlock(l *flock.Flock, log *slog.Logger) {
	if err := l.Unlock(); err != nil {
		log.Warn("could not release workspace lock", "error", err)
	}
}

// openWorkspace resolves the workspace root and, if create is set, creates
// the depot cache directories. Symlinked path components are always refused.
func (s *session) openWorkspace(create bool) error {
	workspace, err := filepath.Abs(s.opts.root)
	if err != nil {
		return err
	}

	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return err
	}

	s.root = workspace

	rootManifest := filepath.Join(workspace, manifestName)
	if err := noSymlinks(workspace, rootManifest); err != nil {
		return err
	}

	if _, err := os.Stat(rootManifest); err != nil {
		return err
	}

	s.cacheDir = filepath.Join(workspace, cacheDirectoryName, depotDirectoryName)
	s.statePath = filepath.Join(s.cacheDir, stateFileName)
	dirs := []string{
		s.cacheDir,
		filepath.Join(s.cacheDir, objectsDirectoryName),
	}

	for _, dir := range dirs {
		if err := noSymlinks(workspace, dir); err != nil {
			return err
		}

		if !create {
			continue
		}

		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	for _, path := range []string{filepath.Join(s.cacheDir, workspaceLockFileName), s.statePath} {
		if err := noSymlinks(workspace, path); err != nil {
			return err
		}
	}

	if create {
		return s.ignoreCache()
	}

	return nil
}

const cacheIgnore = "# Generated by depot: ignore local caches.\n*\n"

// ignoreCache keeps the depot cache out of version control. The file lives in
// the depot directory because .cache/ is shared with tools such as clangd.
func (s *session) ignoreCache() error {
	if err := s.removeLegacyIgnore(); err != nil {
		return err
	}

	path := filepath.Join(s.cacheDir, cacheIgnoreFileName)
	if err := noSymlinks(s.root, path); err != nil {
		return err
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return nil
	}

	if err != nil {
		return err
	}

	_, writeErr := f.WriteString(cacheIgnore)
	return errors.Join(writeErr, f.Close())
}

// removeLegacyIgnore deletes the .cache/.gitignore written by earlier depot
// versions, but only if nobody changed it.
func (s *session) removeLegacyIgnore() error {
	path := filepath.Join(s.root, cacheDirectoryName, cacheIgnoreFileName)
	if err := noSymlinks(s.root, path); err != nil {
		return err
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && string(data) != cacheIgnore) {
		return nil
	}

	if err != nil {
		return err
	}

	return os.Remove(path)
}

// loadVariables reads the saved state and merges in --set assignments.
func (s *session) loadVariables() error {
	saved, err := readState(s.statePath)
	if err != nil {
		return err
	}

	for _, assignment := range s.opts.assignments {
		name, value, ok := strings.Cut(assignment, "=")
		if !ok {
			return fmt.Errorf("--set expects NAME=VALUE, got %q", assignment)
		}

		saved.Variables[name] = value
	}

	vars, err := variables(saved.Variables)
	if err != nil {
		return err
	}

	s.state = saved
	s.vars = vars
	return nil
}

func runConfigure(_ context.Context, s *session) error {
	g := newGraph(s.root, s.vars, s.state.Variables)
	active, err := g.load([]string{filepath.Join(s.root, manifestName)})
	if err != nil {
		return err
	}

	for _, a := range active {
		s.log.Debug("active artifact", "artifact", a.Name, "from", a.From, "to", a.To)
	}

	for _, name := range missingChecksums(active) {
		s.log.Warn("artifact has no sha256; sync will refuse to download it until one is added",
			"artifact", name)
	}

	// configure does not read manifests shipped inside artifacts, which may
	// define required artifacts; sync checks the complete graph.
	if err := g.checkRequired(); err != nil {
		if !mayShipManifests(active) {
			return err
		}

		s.log.Warn("required artifacts are not defined yet; sync fails unless downloaded manifests define them",
			"error", err)
	}

	if err := atomicJSON(s.statePath, s.state); err != nil {
		return err
	}

	s.log.Info("workspace configured",
		"variables", len(s.state.Variables),
		"artifacts", len(g.names))
	return nil
}

func mayShipManifests(active []artifact) bool {
	for _, a := range active {
		if !a.IgnoreDepot {
			return true
		}
	}

	return false
}

// runValidate never touches the network, so the engine has no HTTP client.
func runValidate(ctx context.Context, s *session) error {
	e := &engine{
		root:      s.root,
		statePath: s.statePath,
		state:     s.state,
		jobs:      s.opts.jobs,
		log:       s.log,
		cache: &cache{
			dir: s.cacheDir,
			log: s.log,
		},
	}

	return e.validate(ctx, s.vars)
}

func runSync(ctx context.Context, s *session) error {
	client, closeClient := newHTTPClient(s.opts.jobs, s.opts.timeout)
	defer closeClient()

	e := &engine{
		root:      s.root,
		statePath: s.statePath,
		state:     s.state,
		jobs:      s.opts.jobs,
		log:       s.log,
		cache: &cache{
			dir:    s.cacheDir,
			client: client,
			log:    s.log,
		},
	}

	return e.sync(ctx, s.vars)
}

// newHTTPClient returns a client that never transparently decompresses
// responses (checksums cover the bytes on the wire) and refuses unsafe
// redirects. The returned function releases idle connections.
func newHTTPClient(jobs int, timeout time.Duration) (*http.Client, func()) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.MaxConnsPerHost = jobs

	client := &http.Client{
		Transport:     transport,
		Timeout:       timeout,
		CheckRedirect: checkRedirect,
	}

	return client, transport.CloseIdleConnections
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("too many HTTP redirects")
	}

	if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
		return fmt.Errorf("redirect to unsupported URL scheme")
	}

	if req.URL.User != nil {
		return fmt.Errorf("redirect URL contains credentials")
	}

	if len(via) > 0 && via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing HTTPS to HTTP redirect")
	}

	return nil
}
