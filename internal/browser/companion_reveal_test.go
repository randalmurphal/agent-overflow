package browser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalFilePathForPageURLAcceptsOnlyLocalFiles(t *testing.T) {
	got, err := localFilePathForPageURL("file:///repo/docs/My%20Report.pdf")
	if err != nil || got != filepath.FromSlash("/repo/docs/My Report.pdf") {
		t.Fatalf("file URL = %q, %v, want the percent-decoded path", got, err)
	}
	for _, tc := range []struct{ name, url string }{
		{"remote", "https://example.test/report.pdf"},
		{"blank", "about:blank"},
		{"empty", "   "},
		{"schemeless", "/repo/docs/report.pdf"},
	} {
		if _, err := localFilePathForPageURL(tc.url); err == nil {
			t.Fatalf("%s: %q was accepted, want a refusal naming the limitation", tc.name, tc.url)
		}
	}
	if _, err := localFilePathForPageURL("https://example.test/a.pdf"); err == nil ||
		!strings.Contains(err.Error(), "only local files") {
		t.Fatalf("remote refusal = %v, want it to say only local files qualify", err)
	}
}

func TestRevealPageFileResolvesTheDisplayedFileAndRefusesTheRest(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(real, []byte("%PDF"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "latest.pdf")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	p := &managedPage{id: "page", owner: "thread", ctx: context.Background()}
	var revealed string
	m := &Manager{
		scopes:   map[string]*workspaceScope{"/repo": {pages: map[string]*managedPage{p.id: p}}},
		sessions: map[string]SessionInfo{"thread": {ActivePageID: p.id}},
		revealFileInFileManager: func(_ context.Context, path string) error {
			revealed = path
			return nil
		},
	}
	access := Access{ThreadID: "thread", Workspace: "/repo"}

	// A symlinked address resolves to the real file: the file manager must
	// select the file itself, not a link that may not survive a drag.
	p.setInfo(PageInfo{ID: p.id, URL: "file://" + filepath.ToSlash(link)})
	if err := m.RevealPageFile(context.Background(), access, p.id); err != nil {
		t.Fatalf("reveal = %v", err)
	}
	if resolved, _ := filepath.EvalSymlinks(real); revealed != resolved {
		t.Fatalf("revealed %q, want the resolved %q", revealed, resolved)
	}

	revealed = ""
	p.setInfo(PageInfo{ID: p.id, URL: "https://example.test/report.pdf"})
	if err := m.RevealPageFile(context.Background(), access, p.id); err == nil {
		t.Fatal("a remote page was revealed")
	}
	p.setInfo(PageInfo{ID: p.id, URL: "file://" + filepath.ToSlash(filepath.Join(dir, "missing.pdf"))})
	if err := m.RevealPageFile(context.Background(), access, p.id); err == nil {
		t.Fatal("a missing file was revealed")
	}
	if revealed != "" {
		t.Fatalf("a refused reveal still reached the file manager: %q", revealed)
	}
}

// rendererFSEngine is a stub whose renderer addresses files on a different
// machine: URLs carry the host "renderer.host" and map onto base.
type rendererFSEngine struct {
	browserEngine
	base string
}

func (e rendererFSEngine) FileURL(_ context.Context, path string) (string, error) {
	return "file://renderer.host" + filepath.ToSlash(strings.TrimPrefix(path, e.base)), nil
}

func (e rendererFSEngine) BackendFilePath(_ context.Context, rawURL string) (string, error) {
	rest, ok := strings.CutPrefix(rawURL, "file://renderer.host/")
	if !ok {
		return "", context.Canceled
	}
	return filepath.Join(e.base, filepath.FromSlash(rest)), nil
}

// A page's address on the hosted engine is in the RENDERER's form; the
// reveal must resolve the backend path through the engine seam rather than
// stat the URL's path (live incident 2026-08-31, from the clipboard-copy
// predecessor of this feature).
func TestRevealPageFileResolvesRendererFormURLsThroughTheEngine(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(real, []byte("%PDF"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &managedPage{id: "page", owner: "thread", ctx: context.Background()}
	var revealed string
	m := &Manager{
		engine:   rendererFSEngine{base: dir},
		scopes:   map[string]*workspaceScope{"/repo": {pages: map[string]*managedPage{p.id: p}}},
		sessions: map[string]SessionInfo{"thread": {ActivePageID: p.id}},
		revealFileInFileManager: func(_ context.Context, path string) error {
			revealed = path
			return nil
		},
	}
	access := Access{ThreadID: "thread", Workspace: "/repo"}
	p.setInfo(PageInfo{ID: p.id, URL: "file://renderer.host/report.pdf"})
	if err := m.RevealPageFile(context.Background(), access, p.id); err != nil {
		t.Fatalf("reveal = %v", err)
	}
	if resolved, _ := filepath.EvalSymlinks(real); revealed != resolved {
		t.Fatalf("revealed %q, want the seam-resolved %q", revealed, resolved)
	}
}

func TestWindowsRevealCommandUsesExplorersSelectVerb(t *testing.T) {
	command, err := windowsRevealCommand(`\\wsl.localhost\Distro\home\r\it's [1].pdf`)
	if err != nil {
		t.Fatalf("command = %v", err)
	}
	if command.Name != "explorer.exe" {
		t.Fatalf("command name = %q", command.Name)
	}
	// One argv entry, no shell, no quoting: the comma is Explorer's own
	// argument grammar and the path rides after it verbatim.
	if len(command.Args) != 1 || command.Args[0] != `/select,\\wsl.localhost\Distro\home\r\it's [1].pdf` {
		t.Fatalf("args = %#v", command.Args)
	}
	if _, err := windowsRevealCommand("  "); err == nil {
		t.Fatal("an empty path was accepted")
	}
}

func TestLinuxRevealCommandsSelectThenFallBackToTheParentDirectory(t *testing.T) {
	commands := linuxRevealCommands("/home/r/My Docs/report.pdf")
	if len(commands) != 2 || commands[0].Name != "dbus-send" || commands[1].Name != "xdg-open" {
		t.Fatalf("candidates = %#v, want FileManager1 then xdg-open", commands)
	}
	joined := strings.Join(commands[0].Args, " ")
	if !strings.Contains(joined, "org.freedesktop.FileManager1.ShowItems") ||
		!strings.Contains(joined, "array:string:file:///home/r/My%20Docs/report.pdf") {
		t.Fatalf("dbus args = %v, want ShowItems with a percent-encoded file URI", commands[0].Args)
	}
	if commands[1].Args[len(commands[1].Args)-1] != "/home/r/My Docs" {
		t.Fatalf("fallback args = %v, want the parent directory", commands[1].Args)
	}
}
