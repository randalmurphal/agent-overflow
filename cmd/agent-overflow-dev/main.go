package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultPort         = 9245
	defaultPollInterval = 500 * time.Millisecond
	defaultDebounce     = 750 * time.Millisecond
	defaultStartTimeout = 45 * time.Second
	defaultStopTimeout  = 5 * time.Second
)

type config struct {
	root         string
	port         int
	watch        watchConfig
	pollInterval time.Duration
	debounce     time.Duration
	startTimeout time.Duration
	stopTimeout  time.Duration
}

type watchConfig struct {
	ignoredDirs       map[string]struct{}
	watchedExtensions map[string]struct{}
	useGitIgnore      bool
}

type fileState struct {
	size    int64
	modTime time.Time
}

type snapshot map[string]fileState

type processHandle struct {
	cmd    *exec.Cmd
	done   chan error
	mu     sync.Mutex
	exited bool
}

func main() {
	log.SetFlags(0)

	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		log.Printf("dev: %v", err)
		os.Exit(2)
	}
	if err := run(context.Background(), cfg); err != nil {
		log.Printf("dev: %v", err)
		os.Exit(1)
	}
}

func parseFlags(args []string) (config, error) {
	flagSet := flag.NewFlagSet("agent-overflow-dev", flag.ContinueOnError)
	flagSet.SetOutput(io.Discard)
	port := flagSet.Int("port", envInt("WAILS_VITE_PORT", defaultPort), "frontend dev server port")
	poll := flagSet.Duration("poll", defaultPollInterval, "backend file polling interval")
	debounce := flagSet.Duration("debounce", defaultDebounce, "quiet period before rebuilding after a file change")
	startTimeout := flagSet.Duration("start-timeout", defaultStartTimeout, "timeout waiting for the frontend dev server")
	stopTimeout := flagSet.Duration("stop-timeout", defaultStopTimeout, "timeout before force-killing child process groups")
	if err := flagSet.Parse(args); err != nil {
		return config{}, err
	}
	root, err := os.Getwd()
	if err != nil {
		return config{}, fmt.Errorf("resolve working directory: %w", err)
	}
	if *port <= 0 || *port > 65535 {
		return config{}, fmt.Errorf("invalid port %d", *port)
	}
	if *poll <= 0 {
		return config{}, fmt.Errorf("poll interval must be positive")
	}
	if *debounce < 0 {
		return config{}, fmt.Errorf("debounce must be non-negative")
	}
	if *startTimeout <= 0 {
		return config{}, fmt.Errorf("start timeout must be positive")
	}
	if *stopTimeout <= 0 {
		return config{}, fmt.Errorf("stop timeout must be positive")
	}
	watch, err := loadWatchConfig(root)
	if err != nil {
		return config{}, err
	}
	return config{
		root:         root,
		port:         *port,
		watch:        watch,
		pollInterval: *poll,
		debounce:     *debounce,
		startTimeout: *startTimeout,
		stopTimeout:  *stopTimeout,
	}, nil
}

func run(parent context.Context, cfg config) error {
	if !platformSupportsProcessGroups() {
		return errors.New("agent-overflow-dev is only supported on Unix hosts; use make dev-wsl for the Windows path")
	}

	ctx, stop := signalContext(parent)
	defer stop()

	frontendURL := fmt.Sprintf("http://localhost:%d", cfg.port)
	if err := checkPortAvailable(cfg.port); err != nil {
		return err
	}

	if err := runBuild(ctx, cfg, frontendURL); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	vite, err := startCommand(cfg.root, "frontend", []string{"wails3", "task", "common:dev:frontend"}, childEnv(cfg, frontendURL))
	if err != nil {
		return err
	}
	defer stopProcess(vite, cfg.stopTimeout)

	if err := waitForHTTP(ctx, frontendURL, cfg.startTimeout); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	app, err := startApp(cfg, frontendURL)
	if err != nil {
		return err
	}
	defer func() {
		stopProcess(app, cfg.stopTimeout)
	}()

	current, err := takeSnapshot(cfg.root, cfg.watch)
	if err != nil {
		return err
	}

	ticker := time.NewTicker(cfg.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-app.done:
			app.markExited()
			if ctx.Err() != nil || isExpectedStop(err) {
				return nil
			}
			return fmt.Errorf("app exited: %w", err)
		case err := <-vite.done:
			vite.markExited()
			if ctx.Err() != nil || isExpectedStop(err) {
				return nil
			}
			return fmt.Errorf("frontend dev server exited: %w", err)
		case <-ticker.C:
			next, changed, err := changedSince(cfg.root, cfg.watch, current)
			if err != nil {
				log.Printf("dev: scan failed: %v", err)
				continue
			}
			if !changed {
				continue
			}
			if !waitForQuiet(ctx, cfg.root, cfg.watch, next, cfg.debounce, cfg.pollInterval) {
				return nil
			}
			log.Printf("dev: backend change detected; rebuilding")
			if err := runBuild(ctx, cfg, frontendURL); err != nil {
				log.Printf("dev: build failed: %v", err)
				current, _ = takeSnapshot(cfg.root, cfg.watch)
				continue
			}
			select {
			case err := <-app.done:
				app.markExited()
				if isExpectedStop(err) {
					return nil
				}
				return fmt.Errorf("app exited: %w", err)
			default:
			}
			stopProcess(app, cfg.stopTimeout)
			app, err = startApp(cfg, frontendURL)
			if err != nil {
				return err
			}
			current, err = takeSnapshot(cfg.root, cfg.watch)
			if err != nil {
				return err
			}
		}
	}
}

func runBuild(ctx context.Context, cfg config, frontendURL string) error {
	log.Printf("dev: building backend")
	build, err := startCommand(cfg.root, "build", []string{"wails3", "build", "DEV=true"}, childEnv(cfg, frontendURL))
	if err != nil {
		return err
	}
	var waitErr error
	select {
	case <-ctx.Done():
		stopProcess(build, cfg.stopTimeout)
		return ctx.Err()
	case waitErr = <-build.done:
	}
	build.markExited()
	if waitErr != nil {
		return fmt.Errorf("build failed: %w", waitErr)
	}
	return nil
}

func startApp(cfg config, frontendURL string) (*processHandle, error) {
	log.Printf("dev: starting app")
	return startCommand(cfg.root, "app", []string{"wails3", "task", "run"}, childEnv(cfg, frontendURL))
}

func startCommand(dir, name string, args []string, env []string) (*processHandle, error) {
	if len(args) == 0 {
		return nil, errors.New("empty command")
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	prepareCommand(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	handle := &processHandle{
		cmd:  cmd,
		done: make(chan error, 1),
	}
	go func() {
		handle.done <- cmd.Wait()
	}()
	return handle, nil
}

func stopProcess(p *processHandle, timeout time.Duration) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.exited {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	terminateProcessGroup(p.cmd)
	select {
	case <-p.done:
		p.markExited()
	case <-time.After(timeout):
		killProcessGroup(p.cmd)
		<-p.done
		p.markExited()
	}
}

func (p *processHandle) markExited() {
	p.mu.Lock()
	p.exited = true
	p.mu.Unlock()
}

func childEnv(cfg config, frontendURL string) []string {
	env := os.Environ()
	env = append(env,
		fmt.Sprintf("WAILS_VITE_PORT=%d", cfg.port),
		"FRONTEND_DEVSERVER_URL="+frontendURL,
	)
	return env
}

func waitForHTTP(ctx context.Context, rawURL string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		req, err := http.NewRequestWithContext(waitCtx, http.MethodGet, rawURL, nil)
		if err != nil {
			return fmt.Errorf("build frontend probe: %w", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("frontend dev server did not start at %s within %s: %w", rawURL, timeout, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func checkPortAvailable(port int) error {
	addr := fmt.Sprintf("localhost:%d", port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("frontend port %d is already in use; stop the existing process or rerun with -port: %w", port, err)
	}
	return listener.Close()
}

func changedSince(root string, watch watchConfig, prev snapshot) (snapshot, bool, error) {
	next, err := takeSnapshot(root, watch)
	if err != nil {
		return nil, false, err
	}
	return next, !snapshotsEqual(prev, next), nil
}

func waitForQuiet(ctx context.Context, root string, watch watchConfig, base snapshot, quietFor, pollEvery time.Duration) bool {
	if quietFor == 0 {
		return true
	}
	quietTimer := time.NewTimer(quietFor)
	defer quietTimer.Stop()
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()

	current := base
	for {
		select {
		case <-ctx.Done():
			return false
		case <-quietTimer.C:
			return true
		case <-ticker.C:
			next, changed, err := changedSince(root, watch, current)
			if err != nil {
				log.Printf("dev: debounce scan failed: %v", err)
				continue
			}
			if !changed {
				continue
			}
			current = next
			if !quietTimer.Stop() {
				select {
				case <-quietTimer.C:
				default:
				}
			}
			quietTimer.Reset(quietFor)
		}
	}
}

func takeSnapshot(root string, watch watchConfig) (snapshot, error) {
	out := make(snapshot)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if shouldSkipDir(watch, rel, entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !shouldTrackFile(watch, rel) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		out[rel] = fileState{size: info.Size(), modTime: info.ModTime()}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("snapshot backend files: %w", err)
	}
	out, err = filterGitIgnored(root, watch, out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func shouldSkipDir(watch watchConfig, rel, name string) bool {
	if rel == "." {
		return false
	}
	_, ok := watch.ignoredDirs[name]
	return ok
}

func shouldTrackFile(watch watchConfig, rel string) bool {
	ext := filepath.Ext(rel)
	_, ok := watch.watchedExtensions[ext]
	return ok
}

func snapshotsEqual(a, b snapshot) bool {
	if len(a) != len(b) {
		return false
	}
	for path, left := range a {
		right, ok := b[path]
		if !ok {
			return false
		}
		if left.size != right.size || !left.modTime.Equal(right.modTime) {
			return false
		}
	}
	return true
}

func envInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	out, err := strconv.Atoi(raw)
	if err != nil || out == 0 {
		return fallback
	}
	return out
}

func loadWatchConfig(root string) (watchConfig, error) {
	path := filepath.Join(root, "build", "config.yml")
	body, err := os.ReadFile(path)
	if err != nil {
		return watchConfig{}, fmt.Errorf("read Wails dev config: %w", err)
	}

	var parsed struct {
		DevMode struct {
			Ignore struct {
				Dirs              []string `yaml:"dir"`
				WatchedExtensions []string `yaml:"watched_extension"`
				GitIgnore         bool     `yaml:"git_ignore"`
			} `yaml:"ignore"`
		} `yaml:"dev_mode"`
	}
	if err := yaml.Unmarshal(body, &parsed); err != nil {
		return watchConfig{}, fmt.Errorf("parse Wails dev config: %w", err)
	}

	ignoredDirs := make(map[string]struct{}, len(parsed.DevMode.Ignore.Dirs))
	for _, dir := range parsed.DevMode.Ignore.Dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		ignoredDirs[dir] = struct{}{}
	}

	watchedExtensions := make(map[string]struct{}, len(parsed.DevMode.Ignore.WatchedExtensions))
	for _, pattern := range parsed.DevMode.Ignore.WatchedExtensions {
		ext, err := watchedExtension(pattern)
		if err != nil {
			return watchConfig{}, err
		}
		watchedExtensions[ext] = struct{}{}
	}
	if len(watchedExtensions) == 0 {
		return watchConfig{}, errors.New("Wails dev config has no watched extensions")
	}

	return watchConfig{
		ignoredDirs:       ignoredDirs,
		watchedExtensions: watchedExtensions,
		useGitIgnore:      parsed.DevMode.Ignore.GitIgnore,
	}, nil
}

func watchedExtension(pattern string) (string, error) {
	pattern = strings.TrimSpace(pattern)
	if !strings.HasPrefix(pattern, "*.") {
		return "", fmt.Errorf("unsupported watched extension pattern %q", pattern)
	}
	ext := filepath.Ext(pattern)
	if ext == "" {
		return "", fmt.Errorf("unsupported watched extension pattern %q", pattern)
	}
	return ext, nil
}

func filterGitIgnored(root string, watch watchConfig, files snapshot) (snapshot, error) {
	if !watch.useGitIgnore || len(files) == 0 {
		return files, nil
	}

	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	input := strings.Join(paths, "\n") + "\n"

	cmd := exec.Command("git", "-C", root, "check-ignore", "--stdin")
	cmd.Stdin = strings.NewReader(input)
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return nil, fmt.Errorf("check gitignored backend files: %w", err)
		}
	}
	if len(output) == 0 {
		return files, nil
	}

	ignored := make(map[string]struct{})
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		ignored[filepath.Clean(line)] = struct{}{}
	}

	filtered := make(snapshot, len(files)-len(ignored))
	for path, state := range files {
		if _, ok := ignored[filepath.Clean(path)]; ok {
			continue
		}
		filtered[path] = state
	}
	return filtered, nil
}

func isExpectedStop(err error) bool {
	if err == nil {
		return true
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return false
	}
	return status.Signaled()
}
