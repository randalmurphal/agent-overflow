// Package memory builds what a session receives as memory
// (docs/specs/memory.md): the write rules, the user, project and home
// scopes' indexes, built from their facts' frontmatter at every delivery, an
// assistant's instructions.md and the maintenance warnings of the scopes it
// keeps, as the output of one SessionStart hook (RunHook). The app builds the
// same text (Build) to tell whether a resume must deliver it again (Digest).
//
// Every read goes through an os.Root opened on the folder it reads, so a
// link out of it is never followed, and opens only regular files
// (agenthome.OpenRegular).
package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	// homeFolder is the memory folder of an assistant's home.
	homeFolder = "memory"
	// ProjectPathFile records, in a project's memory folder, the absolute
	// path of the project it belongs to.
	ProjectPathFile = ".project-path"
	// maxKeyBytes bounds a project key, under every file system's 255-byte
	// name limit; a longer key keeps keyHashDigits of its path's hash.
	maxKeyBytes   = 200
	keyHashDigits = 15
)

// Plan is what one session's delivery holds. The app makes it from the
// thread and its assistant; the hook receives it as JSON.
type Plan struct {
	// User is the user scope's folder (UserDir).
	User string `json:"user"`
	// Home is the home of the assistant whose current thread the session
	// is, "" for any other session. It delivers the home's instructions.md
	// and memory, and the maintenance warnings.
	Home string `json:"home,omitempty"`
	// Projects are the projects whose memory the session reads: one is
	// indexed; more, an assistant's, are listed with their folders.
	Projects []Project `json:"projects,omitempty"`
	// ReadOnly is set for a session in read-only mode, which refuses every
	// write: its delivery leaves out the write rules.
	ReadOnly bool `json:"read_only,omitempty"`
}

// Project is a project a delivery reads the memory of.
type Project struct {
	Name string `json:"name"`
	// Path is the project's repository.
	Path string `json:"path"`
	// Dir is its memory folder (ProjectDir).
	Dir string `json:"dir"`
}

// UserDir is the user scope's folder under the app's config directory.
func UserDir(configDir string) string {
	return filepath.Join(configDir, "memory", "user")
}

// ProjectDir is the memory folder of the project at projectPath.
func ProjectDir(configDir, userHome, projectPath string) (string, error) {
	key, err := ProjectKey(userHome, projectPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "memory", "projects", key), nil
}

// HomeDir is the memory folder of an assistant's home.
func HomeDir(home string) string {
	return filepath.Join(home, homeFolder)
}

// ProjectKey names a project's memory folder: the project's path relative to
// userHome with every separator written as `--`, or its whole path the same
// way when it is outside userHome, so that key starts with `--`. Other
// characters stay, so the key reads as the path does. A key over maxKeyBytes
// keeps its start, cut at a character, and ends with `--` and the start of
// the path's SHA-256. Two paths share a key only when a folder name holds
// `--`; ProjectPathFile tells them apart.
func ProjectKey(userHome, projectPath string) (string, error) {
	if !filepath.IsAbs(projectPath) {
		return "", fmt.Errorf("the project path %q is not absolute", projectPath)
	}
	path := filepath.Clean(projectPath)
	if userHome != "" {
		rel, err := filepath.Rel(filepath.Clean(userHome), path)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			path = rel
		}
	}
	key := strings.ReplaceAll(filepath.ToSlash(path), "/", "--")
	if len(key) <= maxKeyBytes {
		return key, nil
	}
	sum := sha256.Sum256([]byte(filepath.Clean(projectPath)))
	keep := maxKeyBytes - len("--") - keyHashDigits
	for keep > 0 && !utf8.RuneStart(key[keep]) {
		keep--
	}
	return key[:keep] + "--" + hex.EncodeToString(sum[:])[:keyHashDigits], nil
}

// Prepare creates the folders plan delivers that do not exist, and records
// each project's path in its folder (ProjectPathFile) when the folder records
// none, before the session that reads them starts: each provider is given
// folders that exist, and a session can write a scope nothing wrote before.
// It returns the folders the session may be given as writable: each one a
// folder, not a link to one, and no project folder that records another
// project, which Build reports as a clash. A folder it cannot create or
// vouch for is left out with an error.
func Prepare(plan Plan) (writable []string, err error) {
	if err := plan.validate(); err != nil {
		return nil, err
	}
	var errs []error
	keep := func(dir string, err error) {
		if err != nil {
			errs = append(errs, err)
			return
		}
		writable = append(writable, dir)
	}
	keep(plan.User, makeFolder(plan.User))
	for _, project := range plan.Projects {
		err := makeFolder(project.Dir)
		if err == nil {
			err = recordProjectPath(project)
		}
		if err == nil && !ownsFolder(project) {
			continue
		}
		keep(project.Dir, err)
	}
	if plan.Home != "" {
		keep(HomeDir(plan.Home), makeHomeFolder(plan.Home))
	}
	return writable, errors.Join(errs...)
}

// errLinked refuses a scope folder that is a link: a session that may
// write where the link is could point it anywhere.
var errLinked = errors.New("is a link, not a folder, so it is not passed to the session as writable")

// makeFolder creates the scope folder dir when it does not exist and checks
// that it is a folder itself, not a link to one.
func makeFolder(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create the memory folder %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("check the memory folder %s: %w", dir, err)
	}
	return folderError(dir, info)
}

// makeHomeFolder is makeFolder for the memory folder of the home, created
// and checked through the home's root, since the assistant and its helpers
// may write the home.
func makeHomeFolder(home string) (err error) {
	dir := HomeDir(home)
	root, err := openFolder(home)
	if err != nil {
		return fmt.Errorf("open the home %s: %w", home, err)
	}
	defer closeInto(root, &err)
	if err := root.Mkdir(homeFolder, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create the memory folder %s: %w", dir, err)
	}
	info, err := root.Lstat(homeFolder)
	if err != nil {
		return fmt.Errorf("check the memory folder %s: %w", dir, err)
	}
	return folderError(dir, info)
}

// folderError refuses info, the Lstat of the scope folder dir, unless it is
// a folder.
func folderError(dir string, info fs.FileInfo) error {
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("the memory folder %s %w", dir, errLinked)
	case !info.IsDir():
		return fmt.Errorf("the memory folder %s is not a folder", dir)
	}
	return nil
}

// ownsFolder reports whether project's folder records project's own path.
func ownsFolder(project Project) bool {
	recorded, err := recordedPath(project.Dir)
	return err == nil && recorded == filepath.Clean(project.Path)
}

// recordProjectPath writes project's path in its folder unless the folder
// records one.
func recordProjectPath(project Project) (err error) {
	name := filepath.Join(project.Dir, ProjectPathFile)
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("record the project's path in %s: %w", name, err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("record the project's path in %s: %w", name, closeErr))
		}
	}()
	if _, err := file.WriteString(filepath.Clean(project.Path) + "\n"); err != nil {
		return fmt.Errorf("record the project's path in %s: %w", name, err)
	}
	return nil
}

// planPath is a path a plan carries, with what it is in an error's words.
type planPath struct{ what, path string }

// validate refuses a plan the hook could not carry or read: every path must
// be absolute, since the hook runs in the session's workspace, valid UTF-8,
// since the plan travels as JSON, and on one line, as a delivery shows it.
func (p Plan) validate() error {
	paths := []planPath{{"the user folder", p.User}}
	if p.Home != "" {
		paths = append(paths, planPath{"the home", p.Home})
	}
	for _, project := range p.Projects {
		if strings.TrimSpace(project.Name) == "" || !utf8.ValidString(project.Name) {
			return fmt.Errorf("the project at %q has no name a delivery can carry", project.Path)
		}
		paths = append(paths,
			planPath{"the project " + project.Name, project.Path},
			planPath{"the memory folder of " + project.Name, project.Dir})
	}
	for _, entry := range paths {
		if !filepath.IsAbs(entry.path) {
			return fmt.Errorf("%s %q is not an absolute path", entry.what, entry.path)
		}
		if !utf8.ValidString(entry.path) {
			return fmt.Errorf("%s %q is not valid UTF-8, which a delivery cannot carry", entry.what, entry.path)
		}
		if strings.ContainsAny(entry.path, "\n\r") {
			return fmt.Errorf("%s %q holds a line break, which a delivery cannot show", entry.what, entry.path)
		}
	}
	return nil
}
