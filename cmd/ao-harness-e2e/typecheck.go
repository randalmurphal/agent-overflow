package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"agent-overflow/internal/atomicfile"
)

// Playwright (and the flow runner's node --experimental-transform-types)
// strip types per file and never check them: a typo'd property inside a
// spec helper's filter or predicate does not throw at runtime, it yields
// undefined and lets an emptiness assertion pass vacuously. So the whole
// suite is typechecked before a run, before the memory reservation, so a
// type error fails in seconds.
//
// A check that passed is recorded in typecheckRecord with a digest of what
// tsc read: every file of e2e/ that git does not ignore (a new spec counts
// before it is added) and every other file tsc listed, its own libraries
// and the frontend sources the suite imports among them. A run whose digest
// matches skips the check. CI checks once in the harness build and packs
// the record with bin/, so no shard checks again; a local run checks after
// any edit.
const typecheckRecord = "bin/e2e-typecheck.json"

type typecheckState struct {
	Digest string   `json:"digest"`
	Files  []string `json:"files"`
}

// ensureTypechecked runs the check unless the record says the suite is
// unchanged since one passed.
func ensureTypechecked(root, testDir string, stdout io.Writer) error {
	if state, err := readTypecheckRecord(root); err == nil {
		digest, err := suiteDigest(root, state.Files)
		if err != nil {
			return err
		}
		if digest == state.Digest {
			fmt.Fprintf(stdout, "ao-harness-e2e: the E2E TypeScript check recorded in %s still holds\n", typecheckRecord)
			return nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return typecheck(root, testDir, stdout)
}

// typecheck runs tsc over the suite and records the pass.
func typecheck(root, testDir string, stdout io.Writer) error {
	cmd := exec.Command("pnpm", "exec", "tsc", "--noEmit", "--listFiles", "-p", ".")
	cmd.Dir = testDir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		stdout.Write(out.Bytes())
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("E2E TypeScript check failed; fix the errors above")
		}
		return fmt.Errorf("run the E2E TypeScript check: %w", err)
	}
	var files []string
	for _, line := range strings.Split(out.String(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, relativeTo(root, line))
		}
	}
	digest, err := suiteDigest(root, files)
	if err != nil {
		return err
	}
	if err := atomicfile.WriteJSON(filepath.Join(root, typecheckRecord), typecheckState{Digest: digest, Files: files}); err != nil {
		return fmt.Errorf("record the E2E TypeScript check: %w", err)
	}
	fmt.Fprintf(stdout, "ao-harness-e2e: E2E TypeScript check passed over %d files\n", len(files))
	return nil
}

func readTypecheckRecord(root string) (typecheckState, error) {
	var state typecheckState
	data, err := os.ReadFile(filepath.Join(root, typecheckRecord))
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil || state.Digest == "" {
		return state, fmt.Errorf("%s is not a typecheck record (%v); delete it", typecheckRecord, err)
	}
	return state, nil
}

// relativeTo writes a path inside root relative to it, so a record made in
// one checkout holds in another at a different path.
func relativeTo(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil && filepath.IsLocal(rel) {
		return filepath.ToSlash(rel)
	}
	return path
}

// suiteDigest hashes the path and content of every file of e2e/ that git
// does not ignore, together with files: tsc's list from the passing check.
func suiteDigest(root string, files []string) (string, error) {
	cmd := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "e2e")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("list the suite's files (git ls-files): %v\n%s", err, stderr.String())
	}
	all := slices.Clone(files)
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel != "" {
			all = append(all, rel)
		}
	}
	slices.Sort(all)
	all = slices.Compact(all)
	digest := sha256.New()
	for _, name := range all {
		path := name
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, filepath.FromSlash(name))
		}
		sum := "missing"
		if data, err := os.ReadFile(path); err == nil {
			content := sha256.Sum256(data)
			sum = hex.EncodeToString(content[:])
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("hash %s for the typecheck record: %w", name, err)
		}
		fmt.Fprintf(digest, "%s\x00%s\n", name, sum)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
