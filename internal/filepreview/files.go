// Package filepreview serves generated pages from a confined directory at an
// independently authenticated origin. It never mounts files on the app's mux.
package filepreview

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"agent-overflow/internal/editor"
)

// resolve selects the workspace for relative assets, or the containing folder
// for a file outside it. Opening through os.Root below is the containment gate;
// a preliminary stat or symlink resolution alone would race filesystem changes.
func resolve(file, workspace string) (directory, target string, err error) {
	abs, err := editor.ResolvePath(file, workspace)
	if err != nil {
		return "", "", err
	}
	switch strings.ToLower(filepath.Ext(abs)) {
	case ".html", ".htm":
	default:
		return "", "", fmt.Errorf("preview requires an HTML file")
	}
	directory = filepath.Dir(abs)
	if workspace != "" {
		if rel, relErr := filepath.Rel(workspace, abs); relErr == nil && filepath.IsLocal(rel) {
			directory = workspace
		}
	}
	rel, err := filepath.Rel(directory, abs)
	if err != nil || !filepath.IsLocal(rel) {
		return "", "", fmt.Errorf("the preview file is outside its directory")
	}
	target = filepath.ToSlash(rel)
	if !allowedPath(target) {
		return "", "", fmt.Errorf("hidden files cannot be previewed")
	}
	return directory, target, nil
}

func allowedPath(name string) bool {
	if !fs.ValidPath(name) || strings.Contains(name, `\`) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}

type files struct{ root *os.Root }

func (f files) check(name string) error {
	file, err := openFile(f.root, name)
	if err != nil {
		return fmt.Errorf("open preview file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("preview requires a regular file")
	}
	return nil
}

func (f files) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Use GET or HEAD to view this preview.", http.StatusMethodNotAllowed)
		return
	}
	// A service worker could outlive this directory and intercept a later
	// preview on a recycled origin. Ordinary scripts and workers still work.
	if r.Header.Get("Service-Worker") != "" || r.Header.Get("Sec-Fetch-Dest") == "serviceworker" {
		http.Error(w, "Service workers are unavailable in file previews.", http.StatusForbidden)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" || strings.HasSuffix(name, "/") {
		name += "index.html"
	}
	if !allowedPath(name) {
		http.NotFound(w, r)
		return
	}
	file, err := openFile(f.root, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, path.Base(name), info.ModTime(), file)
}

func previewPath(name string) string { return (&url.URL{Path: "/" + name}).EscapedPath() }
