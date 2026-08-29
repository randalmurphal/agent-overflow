package compare

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func copyAssets(source, target string) ([]Asset, error) {
	info, err := os.Stat(source)
	if errors.Is(err, os.ErrNotExist) {
		return []Asset{}, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", source)
	}
	var assets []Asset
	err = filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(target, rel), 0o700)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("asset %s is not a regular file", path)
		}
		dest := filepath.Join(target, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			in.Close()
			return err
		}
		n, cpErr := io.Copy(out, in)
		closeErr := out.Close()
		in.Close()
		if cpErr != nil {
			return cpErr
		}
		if closeErr != nil {
			return closeErr
		}
		digest, hashErr := hashFile(dest)
		if hashErr != nil {
			return fmt.Errorf("hash asset %s: %w", path, hashErr)
		}
		assets = append(assets, Asset{Path: filepath.ToSlash(rel), Bytes: n, SHA256: digest})
		return nil
	})
	return assets, err
}

func prefixAssetPaths(assets []Asset, prefix string) {
	for i := range assets {
		assets[i].Path = prefix + assets[i].Path
	}
}

func writeManifest(path string, capsule *Capsule) error {
	capsule.CapsuleSHA256 = ""
	data, err := json.MarshalIndent(capsule, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	capsule.CapsuleSHA256 = hashBytes(data)
	data, err = json.MarshalIndent(capsule, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write capsule manifest: %w", err)
	}
	return nil
}

func makeReadOnly(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.Chmod(path, 0o500)
		}
		return os.Chmod(path, 0o400)
	})
}

func validateCapsule(c Capsule) error {
	if c.Version != CurrentVersion {
		return fmt.Errorf("unsupported capsule version %d", c.Version)
	}
	if c.Database.Path == "" || c.Database.SHA256 == "" {
		return fmt.Errorf("capsule has no database restore input")
	}
	if c.Events.Count != len(c.Events.Events) {
		return fmt.Errorf("event count %d does not match stream %d", c.Events.Count, len(c.Events.Events))
	}
	for i, e := range c.Events.Events {
		if e.Ordinal != i+1 {
			return fmt.Errorf("event ordinal %d is not %d", e.Ordinal, i+1)
		}
	}
	return nil
}
