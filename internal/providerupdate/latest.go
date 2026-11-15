package providerupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Release hosts. Only these URLs are requested; tests and isolated boots
// answer them through the injected HTTP client.
const (
	claudeChannelBase = "https://downloads.claude.ai/claude-code-releases/"
	brewCaskBase      = "https://formulae.brew.sh/api/cask/"
	// CodexNPMLatestURL is the npm registry's manifest for the latest
	// @openai/codex, about 4KB.
	CodexNPMLatestURL = "https://registry.npmjs.org/@openai%2fcodex/latest"
	githubAPIBase     = "https://api.github.com/repos/"
	// CodexLatestReleaseURL is Codex's latest GitHub release, the version
	// the Codex installer installs by default.
	CodexLatestReleaseURL = githubAPIBase + "openai/codex/releases/latest"
)

// Response bounds. A body over its limit fails the request.
const (
	versionBodyLimit  = 4 << 10
	manifestBodyLimit = 1 << 20
	// refsBodyLimit covers the widest listing a range can need: every
	// Codex tag under rust-v0. was about 550KB (1500 refs) in 2026-11.
	refsBodyLimit = 8 << 20
	// releaseBodyLimit bounds one release's JSON. A Codex release was
	// about 350KB in 2026-11, most of it the asset list.
	releaseBodyLimit = 4 << 20
)

const (
	doctorTimeout = 30 * time.Second
	userAgent     = "agent-overflow"
)

// ClaudeChannelURL is where a Claude release channel publishes its version
// as plain text. Only "latest" and "stable" are channels.
func ClaudeChannelURL(channel string) string { return claudeChannelBase + channel }

// BrewCaskURL is Homebrew's JSON description of a cask.
func BrewCaskURL(cask string) string { return brewCaskBase + cask + ".json" }

// Homebrew casks each provider ships as. A brew install ignores Claude's
// settings channel: claude-code tracks stable, claude-code@latest latest.
var providerCasks = map[string][]string{
	ProviderClaude: {"claude-code", "claude-code@latest"},
	ProviderCodex:  {"codex"},
}

// brewInstall names the cask whose Caskroom holds resolvedPath and the brew
// of the Homebrew that owns that Caskroom, "" for an install Homebrew does
// not own. The cask is one of providerCasks, never text from the path.
func brewInstall(providerName, resolvedPath string) (cask, brew string) {
	path := filepath.ToSlash(resolvedPath)
	for _, known := range providerCasks[providerName] {
		if prefix, _, found := strings.Cut(path, "/Caskroom/"+known+"/"); found {
			return known, prefix + "/bin/brew"
		}
	}
	return "", ""
}

// codexStandaloneReleases is the directory, under the Codex home, that the
// Codex installer unpacks each release into.
const codexStandaloneReleases = "/packages/standalone/releases/"

// codexReleaseEnv pins the version the Codex installer installs.
const codexReleaseEnv = "CODEX_RELEASE"

// errNoCodexUpdater fails the check of a Codex install no updater owns.
var errNoCodexUpdater = errors.New("this Codex install has no updater this app can run: it was not installed with npm, Homebrew or the Codex installer")

// plan finds the updater that owns the install, the version it would
// install and the command that installs it:
//
//   - a binary in a Homebrew Caskroom: the cask's version, upgraded by the
//     Homebrew that owns the Caskroom (`claude update` only advises
//     about a brew install);
//   - any other Claude: the version of the release channel `claude doctor`
//     reports, installed by `claude update`;
//   - Codex behind an npm launcher: npm's latest, installed by the
//     launcher's `codex update`;
//   - Codex unpacked by the Codex installer: the latest GitHub release,
//     installed by `codex update` with that release pinned, so the run
//     installs the version this check offered.
//
// Any other Codex has no updater and fails the check.
func (s *Service) plan(ctx context.Context, providerName string, binary Binary, env map[string]string) (latest string, update Command, err error) {
	executable, args, pinRelease := binary.Path, []string{"update"}, false
	cask, brew := brewInstall(providerName, binary.ResolvedPath)
	switch {
	case cask != "":
		latest, err = s.manifestVersion(ctx, BrewCaskURL(cask))
		executable, args = brew, []string{"upgrade", "--cask", cask}
	case providerName == ProviderClaude:
		latest, err = s.claudeLatest(ctx, binary.Path, env)
	case providerName == ProviderCodex && binary.Launcher != "":
		latest, err = s.manifestVersion(ctx, CodexNPMLatestURL)
		executable = binary.Launcher
	case providerName == ProviderCodex && strings.Contains(filepath.ToSlash(binary.ResolvedPath), codexStandaloneReleases):
		latest, err = s.codexLatestRelease(ctx)
		pinRelease = true
	case providerName == ProviderCodex:
		return "", Command{}, errNoCodexUpdater
	default:
		return "", Command{}, unknownProvider(providerName)
	}
	if err != nil {
		return "", Command{}, err
	}
	if pinRelease {
		env = maps.Clone(env)
		if env == nil {
			env = map[string]string{}
		}
		env[codexReleaseEnv] = latest
	}
	update, err = s.command(providerName, executable, env, args...)
	if err != nil {
		return "", Command{}, err
	}
	return latest, update, nil
}

// manifestVersion reads the "version" field of a JSON manifest.
func (s *Service) manifestVersion(ctx context.Context, url string) (string, error) {
	var manifest struct {
		Version string `json:"version"`
	}
	if err := s.getJSON(ctx, url, manifestBodyLimit, &manifest); err != nil {
		return "", err
	}
	return requireVersion(manifest.Version, url)
}

func (s *Service) codexLatestRelease(ctx context.Context) (string, error) {
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := s.getJSON(ctx, CodexLatestReleaseURL, releaseBodyLimit, &release); err != nil {
		return "", err
	}
	version, ok := strings.CutPrefix(release.TagName, releaseRepos[ProviderCodex].tagPrefix)
	if !ok {
		return "", fmt.Errorf("%s answered tag %q, which is not a Codex release tag", CodexLatestReleaseURL, truncate(release.TagName, 64))
	}
	return requireVersion(version, CodexLatestReleaseURL)
}

// claudeLatest is the version of the release channel `claude doctor` says
// the user's settings cascade selects, which is the channel `claude update`
// follows.
func (s *Service) claudeLatest(ctx context.Context, claude string, env map[string]string) (string, error) {
	command, err := s.command(ProviderClaude, claude, env, "doctor")
	if err != nil {
		return "", err
	}
	runCtx, cancel := context.WithTimeout(ctx, doctorTimeout)
	out, err := s.deps.Runner(runCtx, command)
	cancel()
	if err != nil {
		return "", fmt.Errorf("could not run claude doctor: %w", err)
	}
	channel, err := parseDoctorChannel(out.Text)
	if err != nil && out.ExitCode != 0 {
		return "", fmt.Errorf("claude doctor exited with status %d: %w", out.ExitCode, err)
	}
	if err != nil {
		return "", err
	}
	url := ClaudeChannelURL(channel)
	body, err := s.get(ctx, url, versionBodyLimit)
	if err != nil {
		return "", err
	}
	return requireVersion(string(body), url)
}

const doctorChannelLabel = "Auto-update channel:"

// parseDoctorChannel reads the `Auto-update channel:` line of `claude
// doctor` output. Only "latest" and "stable" are accepted, because the
// answer is interpolated into a URL.
func parseDoctorChannel(output string) (string, error) {
	for line := range strings.Lines(stripANSI(output)) {
		_, value, found := strings.Cut(line, doctorChannelLabel)
		if !found {
			continue
		}
		channel := strings.TrimSpace(value)
		if channel != "latest" && channel != "stable" {
			return "", fmt.Errorf("claude doctor reported auto-update channel %q, which is neither latest nor stable", channel)
		}
		return channel, nil
	}
	return "", fmt.Errorf("claude doctor printed no %q line", doctorChannelLabel)
}

// ansiSequence matches CSI and OSC sequences and two-byte escapes.
var ansiSequence = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-_])`)

func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	return ansiSequence.ReplaceAllString(s, "")
}

// errNotFound is a release host's 404, which a notes lookup reads as "no
// release for this tag".
var errNotFound = errors.New("not found")

// get fetches url and returns its body, failing on a non-200 answer or a
// body over limit.
func (s *Service) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", url, err)
	}
	req.Header.Set("User-Agent", userAgent)
	if strings.HasPrefix(url, githubAPIBase) {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	resp, err := s.deps.Client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", url, err)
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if closeErr := resp.Body.Close(); closeErr != nil && readErr == nil {
		readErr = closeErr
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%s answered %s: %w", url, resp.Status, errNotFound)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s answered %s", url, resp.Status)
	case readErr != nil:
		return nil, fmt.Errorf("read %s: %w", url, readErr)
	case int64(len(body)) > limit:
		return nil, fmt.Errorf("%s answered more than %d bytes", url, limit)
	}
	return body, nil
}

func (s *Service) getJSON(ctx context.Context, url string, limit int64, into any) error {
	body, err := s.get(ctx, url, limit)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("decode %s: %w", url, err)
	}
	return nil
}
