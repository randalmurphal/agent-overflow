package git

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"unicode"

	"agent-overflow/internal/transferfiles"
)

// TransferWorkspace carries Git's portable index and the working tree's delta
// from it. Ignored files are local build/configuration state and are not added
// implicitly. Every regular delta is a separately verified archive member.
type TransferWorkspace struct {
	Version int                   `json:"version"`
	Head    string                `json:"head"`
	Branch  string                `json:"branch,omitempty"`
	Index   []TransferIndexEntry  `json:"index"`
	Flags   []TransferIndexFlags  `json:"flags,omitempty"`
	Working []TransferWorkingFile `json:"working,omitempty"`
}

type TransferIndexFlags struct {
	Path            string `json:"path"`
	IntentToAdd     bool   `json:"intentToAdd,omitempty"`
	SkipWorktree    bool   `json:"skipWorktree,omitempty"`
	AssumeUnchanged bool   `json:"assumeUnchanged,omitempty"`
}

type TransferWorkingFile struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // file, symlink, deleted
	Link string `json:"link,omitempty"`
}

// WorkspaceCapture lives only while building one archive. The final check
// compares source Git state and file identities after all bytes have streamed;
// an external edit causes a retry, never an internally inconsistent snapshot.
type WorkspaceCapture struct {
	Workspace TransferWorkspace
	Sources   []transferfiles.Source
	stamps    []fs.FileInfo
}

func transferWorkspacePath(name string) bool {
	if !transferfiles.ValidName(name) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		// HFS ignores some formatting characters in names. NTFS may expose
		// the metadata directory through a DOS short name. Neither spelling
		// is ordinary transferable working content.
		part = strings.ToLower(strings.Map(func(r rune) rune {
			if unicode.Is(unicode.Cf, r) {
				return -1
			}
			return r
		}, part))
		if part == ".git" || strings.HasPrefix(part, "git~") || strings.HasPrefix(part, ".git~") {
			return false
		}
	}
	return true
}

func TransferWorkingSourceName(index int) string { return fmt.Sprintf("workspace/working/%06d", index) }

func (c *Core) transferPathList(ctx context.Context, cwd string, args ...string) ([]string, error) {
	output, _, err := c.executeSpec(commandSpec{binary: "git", cwd: cwd, ctx: ctx, args: args, maxBytes: maxTransferIndexBytes})
	if err != nil {
		return nil, err
	}
	var names []string
	for name := range strings.SplitSeq(output, "\x00") {
		if name == "" {
			continue
		}
		if !transferWorkspacePath(name) || len(names) >= maxTransferIndexEntries {
			return nil, errors.New("transfer: unsupported workspace path or entry count")
		}
		names = append(names, name)
	}
	return names, nil
}

func (c *Core) readTransferWorkspace(ctx context.Context, cwd string) (TransferWorkspace, []string, error) {
	w := TransferWorkspace{Version: 1}
	head, _, err := c.executeSpec(commandSpec{binary: "git", cwd: cwd, ctx: ctx, args: []string{"rev-parse", "--verify", "HEAD^{commit}"}})
	if err != nil {
		return w, nil, fmt.Errorf("Commit the repository's initial state before transferring its workspace: %w", err)
	}
	w.Head = strings.TrimSpace(head)
	if !validTransferOID(w.Head) {
		return w, nil, errors.New("transfer: invalid workspace commit")
	}
	branch, err := c.runSpec(commandSpec{binary: "git", cwd: cwd, ctx: ctx, args: []string{"symbolic-ref", "--quiet", "--short", "HEAD"}})
	if err != nil {
		return w, nil, err
	}
	if branch.exitCode != 0 && branch.exitCode != 1 {
		return w, nil, errors.New("transfer: could not read the workspace branch")
	}
	w.Branch = strings.TrimSpace(branch.stdout)
	w.Index, err = c.ReadTransferIndex(ctx, cwd)
	if err != nil {
		return w, nil, err
	}
	for _, entry := range w.Index {
		if !transferWorkspacePath(entry.Path) {
			return w, nil, errors.New("transfer: workspace index contains a reserved path")
		}
		if entry.Mode == "160000" {
			return w, nil, errors.New("This workspace contains submodules. Transfer the conversation without workspace changes, or use a checkout without submodules.")
		}
	}
	w.Flags, err = c.readTransferIndexFlags(ctx, cwd)
	if err != nil {
		return w, nil, err
	}
	changed, err := c.transferPathList(ctx, cwd, "diff-files", "--name-only", "--no-ext-diff", "--no-textconv", "-z", "--ignore-submodules=none")
	if err != nil {
		return w, nil, err
	}
	untracked, err := c.transferPathList(ctx, cwd, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return w, nil, err
	}
	changed = append(changed, untracked...)
	for _, flag := range w.Flags {
		changed = append(changed, flag.Path)
	}
	converted, err := c.transferConvertedWorkingPaths(ctx, cwd, w.Index, changed)
	if err != nil {
		return w, nil, err
	}
	changed = append(changed, converted...)
	slices.Sort(changed)
	changed = slices.Compact(changed)
	if len(changed) > transferfiles.MaxFiles {
		return w, nil, errors.New("transfer: too many working files")
	}
	return w, changed, nil
}

// CaptureTransferWorkspace does not write the source checkout or index. The
// caller builds its archive from Sources then calls VerifyTransferWorkspace
// before publishing that archive's durable completion marker.
func (c *Core) CaptureTransferWorkspace(ctx context.Context, cwd string) (WorkspaceCapture, error) {
	w, paths, err := c.readTransferWorkspace(ctx, cwd)
	if err != nil {
		return WorkspaceCapture{}, err
	}
	root, err := os.OpenRoot(cwd)
	if err != nil {
		return WorkspaceCapture{}, err
	}
	defer root.Close()
	capture := WorkspaceCapture{Workspace: w}
	var total int64
	for i, path := range paths {
		if err := ctx.Err(); err != nil {
			return WorkspaceCapture{}, err
		}
		file, stamp, err := readWorkingEntry(root, path)
		if err != nil {
			return WorkspaceCapture{}, err
		}
		capture.Workspace.Working = append(capture.Workspace.Working, file)
		capture.stamps = append(capture.stamps, stamp)
		if file.Kind == "file" {
			total += stamp.Size()
			if stamp.Size() > transferfiles.MaxFileBytes || total > transferfiles.MaxTotalBytes {
				return WorkspaceCapture{}, errors.New("transfer: working files exceed the size limit")
			}
			capture.Sources = append(capture.Sources, transferfiles.Source{Root: cwd, Path: path, Name: TransferWorkingSourceName(i)})
		}
	}
	return capture, nil
}

func readWorkingEntry(root *os.Root, path string) (TransferWorkingFile, fs.FileInfo, error) {
	file := TransferWorkingFile{Path: path, Kind: "deleted"}
	parts := strings.Split(path, "/")
	for i := range len(parts) - 1 {
		info, err := root.Lstat(filepath.FromSlash(strings.Join(parts[:i+1], "/")))
		if errors.Is(err, fs.ErrNotExist) {
			return file, nil, nil
		}
		if err != nil {
			return file, nil, err
		}
		if !info.IsDir() {
			// A parent replaced by a file/link makes this indexed child absent.
			// Record the deletion without following the replacement.
			return file, nil, nil
		}
	}
	info, err := root.Lstat(filepath.FromSlash(path))
	if errors.Is(err, fs.ErrNotExist) {
		return file, nil, nil
	}
	if err != nil {
		return file, nil, err
	}
	switch {
	case info.Mode().IsRegular():
		file.Kind = "file"
	case info.Mode()&os.ModeSymlink != 0:
		file.Kind = "symlink"
		file.Link, err = root.Readlink(filepath.FromSlash(path))
		if err != nil {
			return file, nil, err
		}
		if len(file.Link) > 4096 || strings.ContainsRune(file.Link, 0) {
			return file, nil, errors.New("transfer: unsupported symbolic link")
		}
	case info.IsDir():
		// A tracked file replaced by a directory is deleted from the index's
		// working view; its untracked children have their own archive entries.
	default:
		return file, nil, fmt.Errorf("transfer: unsupported working file: %s", path)
	}
	return file, info, nil
}

func (c *Core) VerifyTransferWorkspace(ctx context.Context, cwd string, capture WorkspaceCapture) error {
	w, paths, err := c.readTransferWorkspace(ctx, cwd)
	if err != nil {
		return err
	}
	expected := capture.Workspace
	expected.Working = nil
	if !reflect.DeepEqual(w, expected) || len(paths) != len(capture.stamps) || len(paths) != len(capture.Workspace.Working) {
		return errors.New("The workspace changed during transfer preparation. Retry to capture its current state.")
	}
	root, err := os.OpenRoot(cwd)
	if err != nil {
		return err
	}
	defer root.Close()
	for i, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, stamp, err := readWorkingEntry(root, path)
		if err != nil {
			return err
		}
		before := capture.stamps[i]
		changed := (before == nil) != (stamp == nil)
		if before != nil && stamp != nil {
			changed = !os.SameFile(before, stamp) || before.Mode() != stamp.Mode() || before.Size() != stamp.Size() || !before.ModTime().Equal(stamp.ModTime())
		}
		if changed || file != capture.Workspace.Working[i] {
			return errors.New("The workspace changed during transfer preparation. Retry to capture its current state.")
		}
	}
	return nil
}
