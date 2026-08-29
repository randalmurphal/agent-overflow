package compare

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Load reads and verifies a capsule before a run can touch a disposable
// target. Verification covers the manifest digest and every content hash.
func Load(path string) (Capsule, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return Capsule{}, fmt.Errorf("resolve capsule manifest %s: %w", path, err)
	}
	if err := refuseSymlinkPath(path); err != nil {
		return Capsule{}, err
	}
	manifestInfo, err := os.Lstat(path)
	if err != nil {
		return Capsule{}, fmt.Errorf("inspect capsule manifest %s: %w", path, err)
	}
	if !manifestInfo.Mode().IsRegular() {
		return Capsule{}, fmt.Errorf("capsule manifest %s is not a regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Capsule{}, fmt.Errorf("read capsule manifest %s: %w", path, err)
	}
	var c Capsule
	if err := json.Unmarshal(data, &c); err != nil {
		return Capsule{}, fmt.Errorf("parse capsule manifest: %w", err)
	}
	if err := validateCapsule(c); err != nil {
		return Capsule{}, err
	}
	stored := c.CapsuleSHA256
	c.CapsuleSHA256 = ""
	canonical, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return Capsule{}, err
	}
	canonical = append(canonical, '\n')
	if stored == "" || stored != hashBytes(canonical) {
		return Capsule{}, fmt.Errorf("capsule manifest digest mismatch")
	}
	c.CapsuleSHA256 = stored
	c.manifestPath = path
	root := filepath.Dir(path)
	for _, asset := range append([]Asset{c.Database}, append(c.Attachments, c.Fixtures...)...) {
		full, pathErr := capsuleAssetPath(root, asset.Path)
		if pathErr != nil {
			return Capsule{}, fmt.Errorf("capsule asset %s: %w", asset.Path, pathErr)
		}
		info, statErr := os.Lstat(full)
		if statErr != nil {
			return Capsule{}, fmt.Errorf("capsule asset %s: %w", asset.Path, statErr)
		}
		if !info.Mode().IsRegular() {
			return Capsule{}, fmt.Errorf("capsule asset %s is not a regular file", asset.Path)
		}
		digest, hashErr := hashFile(full)
		if hashErr != nil {
			return Capsule{}, fmt.Errorf("hash capsule asset %s: %w", asset.Path, hashErr)
		}
		if digest != asset.SHA256 {
			return Capsule{}, fmt.Errorf("capsule asset %s hash mismatch", asset.Path)
		}
		if info.Size() != asset.Bytes {
			return Capsule{}, fmt.Errorf("capsule asset %s size mismatch", asset.Path)
		}
	}
	eventPath, pathErr := capsuleAssetPath(root, c.Events.Path)
	if pathErr != nil {
		return Capsule{}, fmt.Errorf("capsule event path %s: %w", c.Events.Path, pathErr)
	}
	eventInfo, statErr := os.Lstat(eventPath)
	if statErr != nil {
		return Capsule{}, fmt.Errorf("capsule event stream: %w", statErr)
	}
	if !eventInfo.Mode().IsRegular() {
		return Capsule{}, fmt.Errorf("capsule event stream is not a regular file")
	}
	digest, hashErr := hashFile(eventPath)
	if hashErr != nil {
		return Capsule{}, fmt.Errorf("hash capsule event stream: %w", hashErr)
	}
	if digest != c.Events.SHA256 {
		return Capsule{}, fmt.Errorf("capsule event stream hash mismatch")
	}
	if err := validateEventFile(eventPath, c.Events.Events); err != nil {
		return Capsule{}, err
	}
	return c, nil
}

// capsuleAssetPath rejects links in every existing component. A lexical
// relative path is not sufficient because a capsule may have been supplied by
// another process, and opening a linked parent would read arbitrary host
// files before the content hash gets a chance to run.
func capsuleAssetPath(root, relative string) (string, error) {
	if !safeRelative(relative) {
		return "", errors.New("path escapes capsule")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	full := filepath.Clean(filepath.Join(root, filepath.FromSlash(relative)))
	if !underCapsulePath(full, root) {
		return "", errors.New("path escapes capsule")
	}
	for current := full; ; current = filepath.Dir(current) {
		info, statErr := os.Lstat(current)
		if statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return "", errors.New("symlink is not allowed")
			}
		} else if !os.IsNotExist(statErr) {
			return "", statErr
		}
		if current == root {
			break
		}
		next := filepath.Dir(current)
		if next == current || !underCapsulePath(next, root) {
			return "", errors.New("path escapes capsule")
		}
	}
	return full, nil
}

func underCapsulePath(path, root string) bool {
	path, root = filepath.Clean(path), filepath.Clean(root)
	return path == root || (len(path) > len(root) && strings.HasPrefix(path, root+string(filepath.Separator)))
}

func validateEventFile(path string, expected []Event) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open capsule event stream: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for i := 0; i < len(expected); i++ {
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("read capsule event stream: %w", err)
			}
			return fmt.Errorf("capsule event stream ended at ordinal %d", i+1)
		}
		var got Event
		if err := json.Unmarshal(scanner.Bytes(), &got); err != nil {
			return fmt.Errorf("parse capsule event %d: %w", i+1, err)
		}
		if got.Ordinal != expected[i].Ordinal || got.Timestamp != expected[i].Timestamp || got.ThreadID != expected[i].ThreadID || got.Kind != expected[i].Kind || !sameJSON(got.Data, expected[i].Data) || got.SHA256 != expected[i].SHA256 {
			return fmt.Errorf("capsule event %d does not match manifest", i+1)
		}
	}
	if scanner.Scan() {
		return fmt.Errorf("capsule event stream has more events than manifest")
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read capsule event stream: %w", err)
	}
	return nil
}

func sameJSON(a, b []byte) bool {
	var left, right bytes.Buffer
	if json.Compact(&left, a) != nil || json.Compact(&right, b) != nil {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(left.Bytes(), right.Bytes())
}
