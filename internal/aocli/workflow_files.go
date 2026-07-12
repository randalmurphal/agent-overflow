package aocli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func writeScaffold(files []scaffoldFile, configRoot, targetDir string) error {
	if _, err := inspectScopeDir(configRoot, targetDir, false); err != nil {
		return err
	}
	if err := checkScaffoldDestinations(files); err != nil {
		return err
	}
	if _, err := inspectScopeDir(configRoot, targetDir, true); err != nil {
		return err
	}
	scopeRoot, err := openNestedRoot(configRoot, targetDir)
	if err != nil {
		return err
	}
	created := make([]scaffoldFile, 0, len(files))
	var writeErr error
	for _, file := range files {
		if _, err := inspectScopeDir(configRoot, targetDir, false); err != nil {
			writeErr = err
			break
		}
		if err := writeExclusiveAt(scopeRoot, filepath.Base(file.path), file.path, file.data); err != nil {
			writeErr = err
			break
		}
		created = append(created, file)
	}
	if writeErr != nil {
		writeErr = errors.Join(writeErr, removeCreatedAt(scopeRoot, created))
	}
	return errors.Join(writeErr, wrapRootCloseError(targetDir, scopeRoot.Close()))
}

func checkScaffoldDestinations(files []scaffoldFile) error {
	for _, file := range files {
		if _, err := os.Lstat(file.path); err == nil {
			return fmt.Errorf("refusing to overwrite existing file %q", file.path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect scaffold destination %q: %w", file.path, err)
		}
	}
	return nil
}

// inspectScopeDir verifies every target component below configRoot without
// following symlinks. configRoot itself may intentionally be a symlink.
func inspectScopeDir(configRoot, targetDir string, create bool) (bool, error) {
	absRoot, relative, err := relativeToConfigRoot(configRoot, targetDir)
	if err != nil {
		return false, err
	}
	if create {
		if err := os.MkdirAll(absRoot, 0o700); err != nil {
			return false, fmt.Errorf("create config root %q: %w", configRoot, err)
		}
		root, err := os.OpenRoot(absRoot)
		if err != nil {
			return false, fmt.Errorf("open config root %q: %w", configRoot, err)
		}
		mkdirErr := root.MkdirAll(relative, 0o700)
		closeErr := root.Close()
		if mkdirErr != nil || closeErr != nil {
			var createErr error
			if mkdirErr != nil {
				createErr = fmt.Errorf("create workflow scope directory %q: %w", targetDir, mkdirErr)
			}
			return false, errors.Join(
				createErr,
				wrapRootCloseError(configRoot, closeErr),
			)
		}
	}
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		if !create && errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("resolve config root %q: %w", configRoot, err)
	}
	current := resolvedRoot
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("inspect workflow scope component %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("workflow scope component %q must not be a symlink", current)
		}
		if !info.IsDir() {
			return false, fmt.Errorf("workflow scope component %q is not a directory", current)
		}
	}
	return true, nil
}

func relativeToConfigRoot(configRoot, target string) (string, string, error) {
	absRoot, err := filepath.Abs(configRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolve config root %q: %w", configRoot, err)
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", "", fmt.Errorf("resolve path %q: %w", target, err)
	}
	relative, err := filepath.Rel(absRoot, absTarget)
	if err != nil || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path %q is outside config root %q", target, configRoot)
	}
	return absRoot, relative, nil
}

func openNestedRoot(configRoot, targetDir string) (*os.Root, error) {
	absRoot, relative, err := relativeToConfigRoot(configRoot, targetDir)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(absRoot)
	if err != nil {
		return nil, fmt.Errorf("open config root %q: %w", configRoot, err)
	}
	if relative == "." {
		return root, nil
	}
	nested, openErr := root.OpenRoot(relative)
	closeErr := root.Close()
	if openErr != nil || closeErr != nil {
		if nested != nil {
			_ = nested.Close()
		}
		return nil, errors.Join(
			wrapRootOpenError(targetDir, openErr),
			wrapRootCloseError(configRoot, closeErr),
		)
	}
	return nested, nil
}

func writeExclusiveAt(root *os.Root, name, displayPath string, data []byte) error {
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("refusing to overwrite existing file %q", displayPath)
		}
		return fmt.Errorf("create workflow scaffold file %q: %w", displayPath, err)
	}
	if _, err := file.Write(data); err != nil {
		closeErr := file.Close()
		removeErr := root.Remove(name)
		return errors.Join(
			fmt.Errorf("write workflow scaffold file %q: %w", displayPath, err),
			wrapCleanupError(displayPath, "close", closeErr),
			wrapCleanupError(displayPath, "remove", removeErr),
		)
	}
	if err := file.Close(); err != nil {
		removeErr := root.Remove(name)
		return errors.Join(
			fmt.Errorf("close workflow scaffold file %q: %w", displayPath, err),
			wrapCleanupError(displayPath, "remove", removeErr),
		)
	}
	return nil
}

func wrapCleanupError(path, action string, err error) error {
	if err == nil || action == "remove" && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("%s partial scaffold file %q: %w", action, path, err)
}

func removeCreatedAt(root *os.Root, files []scaffoldFile) error {
	var cleanupErr error
	for i := len(files) - 1; i >= 0; i-- {
		if err := root.Remove(filepath.Base(files[i].path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove partial scaffold file %q: %w", files[i].path, err))
		}
	}
	return cleanupErr
}

func wrapRootOpenError(path string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("open confined directory %q: %w", path, err)
}

func wrapRootCloseError(path string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("close confined directory %q: %w", path, err)
}

func removeConfinedFile(configRoot, dir, name, displayPath string) error {
	root, err := openNestedRoot(configRoot, dir)
	if err != nil {
		return err
	}
	removeErr := root.Remove(name)
	return errors.Join(
		wrapCleanupError(displayPath, "remove", removeErr),
		wrapRootCloseError(dir, root.Close()),
	)
}
