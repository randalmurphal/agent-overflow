package browser

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"agent-overflow/internal/platform"
	"agent-overflow/internal/safecopy"
)

// clipboardStagingDir is the subdirectory of the Windows temp directory WSL
// file copies are staged into. Sandboxed Windows apps (Teams, Outlook) cannot
// read a \\wsl.localhost UNC path out of the clipboard, so the bytes have to
// live on a real Windows volume before Set-Clipboard names them.
const clipboardStagingDir = "agent-overflow-clipboard"

// clipboardCommand is one fully-formed OS command that places a file on the
// system clipboard. Keeping the argv a value rather than an exec.Cmd is what
// lets the construction rules be unit-tested without running anything.
type clipboardCommand struct {
	Name  string
	Args  []string
	Stdin string
}

// CopyPageFileToClipboard puts the local file a page is displaying onto the
// user's OS clipboard as a FILE, not as its path text, so a paste into a chat
// or a mail client attaches it. Only file:// pages qualify: there is nothing to
// copy for a remote URL, and downloading one silently would be a surprise.
func (m *Manager) CopyPageFileToClipboard(ctx context.Context, access Access, pageID string) error {
	p, _, err := m.lookupOrSelectPage(ctx, access, pageID)
	if err != nil {
		return err
	}
	path, err := localFilePathForPageURL(p.cachedInfo().URL)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("browser: resolve %s for clipboard: %w", path, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("browser: read %s for clipboard: %w", resolved, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("browser: only a regular file can be copied to the clipboard, %s is not one", resolved)
	}
	if m.copyFileToOSClipboard != nil {
		return m.copyFileToOSClipboard(ctx, resolved)
	}
	return copyFileToOSClipboard(ctx, resolved)
}

// localFilePathForPageURL turns a page's current address into the local path it
// is rendering, and rejects everything that is not a local file.
func localFilePathForPageURL(rawURL string) (string, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return "", fmt.Errorf("browser: page has no address to copy")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || !strings.EqualFold(parsed.Scheme, "file") {
		return "", fmt.Errorf("browser: only local files can be copied to the clipboard, this page is at %s", trimmed)
	}
	if parsed.Path == "" {
		return "", fmt.Errorf("browser: page address %s names no file", trimmed)
	}
	return filepath.FromSlash(parsed.Path), nil
}

// copyFileToOSClipboard is the production hand-off. Every branch ends in one
// bounded subprocess; nothing here is reachable from a test, which goes through
// Manager.copyFileToOSClipboard instead.
func copyFileToOSClipboard(ctx context.Context, path string) error {
	switch {
	case platform.IsWSL():
		staged, err := stageClipboardFileForWindows(ctx, path)
		if err != nil {
			return err
		}
		command, err := windowsClipboardCommand(staged)
		if err != nil {
			return err
		}
		return runClipboardCommand(ctx, command)
	case runtime.GOOS == "darwin":
		command, err := macClipboardCommand(path)
		if err != nil {
			return err
		}
		return runClipboardCommand(ctx, command)
	default:
		return runLinuxClipboardCommands(ctx, path)
	}
}

// windowsClipboardCommand names the STAGED Windows-side copy. Set-Clipboard is
// given -LiteralPath rather than -Path because a staged name inherits the
// source file's name, and `[` in a file name is a wildcard to -Path.
func windowsClipboardCommand(windowsPath string) (clipboardCommand, error) {
	if strings.TrimSpace(windowsPath) == "" {
		return clipboardCommand{}, fmt.Errorf("browser: empty Windows clipboard path")
	}
	if strings.ContainsAny(windowsPath, "\r\n") {
		return clipboardCommand{}, fmt.Errorf("browser: refusing to copy a path containing a line break")
	}
	// PowerShell single-quoted literals escape a quote by doubling it, and
	// escape nothing else — so this is the whole rule, not a partial one.
	quoted := "'" + strings.ReplaceAll(windowsPath, "'", "''") + "'"
	return clipboardCommand{
		Name: "powershell.exe",
		Args: []string{"-NoProfile", "-NonInteractive", "-Command", "Set-Clipboard -LiteralPath " + quoted},
	}, nil
}

// macClipboardCommand builds the AppleScript that puts a POSIX file on the
// pasteboard. AppleScript string literals escape with backslashes, so rather
// than reimplementing that grammar the two characters that would need it are
// refused outright — a macOS path may legitimately contain neither in practice.
func macClipboardCommand(path string) (clipboardCommand, error) {
	if strings.ContainsAny(path, "\"\\\r\n") {
		return clipboardCommand{}, fmt.Errorf("browser: cannot copy %s: its name contains a character AppleScript cannot quote", path)
	}
	return clipboardCommand{
		Name: "osascript",
		Args: []string{"-e", `set the clipboard to (POSIX file "` + path + `")`},
	}, nil
}

// linuxClipboardCommands is the ordered candidate list for a native Linux
// desktop: Wayland first, then X11. Both take a text/uri-list on stdin, which
// is the target every file manager and browser reads a file paste from.
func linuxClipboardCommands(path string) []clipboardCommand {
	uri := (&url.URL{Scheme: "file", Path: path}).String() + "\n"
	return []clipboardCommand{
		{Name: "wl-copy", Args: []string{"-t", "text/uri-list"}, Stdin: uri},
		{Name: "xclip", Args: []string{"-selection", "clipboard", "-t", "text/uri-list"}, Stdin: uri},
	}
}

func runLinuxClipboardCommands(ctx context.Context, path string) error {
	var lastErr error
	for _, command := range linuxClipboardCommands(path) {
		if _, err := exec.LookPath(command.Name); err != nil {
			lastErr = err
			continue
		}
		if err := runClipboardCommand(ctx, command); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no clipboard tool found")
	}
	return fmt.Errorf("browser: copy file to clipboard needs wl-copy (Wayland) or xclip (X11) on PATH: %w", lastErr)
}

func runClipboardCommand(ctx context.Context, command clipboardCommand) error {
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, command.Name, command.Args...)
	if command.Stdin != "" {
		cmd.Stdin = strings.NewReader(command.Stdin)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("browser: %s clipboard copy failed: %w: %s", command.Name, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// stageClipboardFileForWindows copies the file onto a real Windows volume and
// returns the Windows path of the copy. Set-Clipboard on a \\wsl.localhost UNC
// path succeeds and then pastes as nothing in sandboxed apps, which cannot
// reach that provider — so staging is the feature, not an optimization.
func stageClipboardFileForWindows(ctx context.Context, path string) (string, error) {
	tempDir, err := windowsTempDirLinuxPath(ctx)
	if err != nil {
		return "", err
	}
	stagingRoot := filepath.Join(tempDir, clipboardStagingDir)
	if err := os.MkdirAll(stagingRoot, 0o755); err != nil {
		return "", fmt.Errorf("browser: create clipboard staging directory: %w", err)
	}
	staged, err := stageClipboardFile(path, stagingRoot)
	if err != nil {
		return "", err
	}
	return windowsPathFor(ctx, staged)
}

// stageClipboardFile copies one regular file into an existing staging root
// under its own base name and returns the Linux path of the copy. The copy goes
// through safecopy so a symlinked component cannot redirect the write and a
// crash cannot leave a half-written file under a real name.
func stageClipboardFile(sourcePath, stagingRoot string) (string, error) {
	base := filepath.Base(sourcePath)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return "", fmt.Errorf("browser: %s has no file name to stage", sourcePath)
	}
	destination := filepath.Join(stagingRoot, base)
	if err := safecopy.File(filepath.Dir(sourcePath), base, stagingRoot, base, 0o644); err != nil {
		return "", fmt.Errorf("browser: stage %s for the Windows clipboard: %w", sourcePath, err)
	}
	return destination, nil
}

// windowsTempDirLinuxPath resolves %TEMP% on the Windows side and converts it
// to the WSL path that names the same directory.
func windowsTempDirLinuxPath(ctx context.Context) (string, error) {
	windowsTemp, err := windowsTempDir(ctx)
	if err != nil {
		return "", err
	}
	converted, err := runWSLPath(ctx, "-u", windowsTemp)
	if err != nil {
		return "", fmt.Errorf("browser: convert Windows temp directory %q: %w", windowsTemp, err)
	}
	return converted, nil
}

func windowsTempDir(ctx context.Context) (string, error) {
	candidates := []clipboardCommand{
		{Name: "cmd.exe", Args: []string{"/c", "echo %TEMP%"}},
		{Name: "powershell.exe", Args: []string{"-NoProfile", "-NonInteractive", "-Command", "[IO.Path]::GetTempPath()"}},
	}
	var lastErr error
	for _, command := range candidates {
		output, err := runWindowsInterop(ctx, command)
		if err != nil {
			lastErr = err
			continue
		}
		temp := parseWindowsTempOutput(output)
		if temp == "" {
			lastErr = fmt.Errorf("%s returned no temp directory", command.Name)
			continue
		}
		return temp, nil
	}
	return "", fmt.Errorf("browser: resolve the Windows temp directory: %w", lastErr)
}

// parseWindowsTempOutput takes the last non-empty line of a Windows interop
// command's output. cmd.exe prepends a UNC-current-directory warning when it is
// launched from a Linux path, and an unexpanded `%TEMP%` means the variable was
// not set at all.
func parseWindowsTempOutput(output string) string {
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.Contains(line, "%TEMP%") {
			continue
		}
		return strings.TrimRight(line, `\`)
	}
	return ""
}

func runWindowsInterop(ctx context.Context, command clipboardCommand) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, command.Name, command.Args...)
	// cmd.exe refuses a Linux working directory and says so on stdout. Give it
	// one it accepts so the answer is the only thing on the last line.
	if _, err := os.Stat("/mnt/c"); err == nil {
		cmd.Dir = "/mnt/c"
	}
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", command.Name, err)
	}
	return string(output), nil
}

func runWSLPath(ctx context.Context, args ...string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(runCtx, "wslpath", args...).Output()
	if err != nil {
		return "", fmt.Errorf("wslpath: %w", err)
	}
	converted := strings.TrimSpace(string(output))
	if converted == "" {
		return "", fmt.Errorf("wslpath returned nothing")
	}
	return converted, nil
}

func windowsPathFor(ctx context.Context, linuxPath string) (string, error) {
	converted, err := runWSLPath(ctx, "-w", linuxPath)
	if err != nil {
		return "", fmt.Errorf("browser: convert %s to a Windows path: %w", linuxPath, err)
	}
	return converted, nil
}
