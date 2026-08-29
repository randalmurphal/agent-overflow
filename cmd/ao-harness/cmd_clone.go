package main

// clone builds a harness data root from a COPY of a real app data dir. The
// database is snapshotted consistently, provider/session handles are scrubbed,
// and attachments are copied without following links. Provider homes and
// settings are intentionally left to the normal isolated harness boot path.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/harness/instanceinfo"
)

const storeFileName = "agent-overflow.db"
const attachmentsDirName = "attachments"
const cloneSuffix = "-clone"

var cloneExclusions = []string{
	"settings.json",
	"provider-accounts.json",
	"replay/",
	"ui-trace/",
	"design-workdirs/",
	"logs/",
	"account-audit.log",
	"usage-backoff.json",
	"harness-instance.json",
}

func runClone(e *env, args []string) error {
	flags := e.newFlagSet("clone")
	from := flags.String("from", "", "the app data dir to copy (the real one — that is the point of this verb)")
	dataDir := flags.String("data-dir", "", "data root to build (default: this worktree's default root plus \""+cloneSuffix+"\")")
	force := flags.Bool("force", false, "replace an existing database at the target instead of refusing")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("clone takes no positional arguments (got %v)", rest)
	}
	if strings.TrimSpace(*from) == "" {
		return usagef("clone needs --from <app data dir> (the directory holding %s, or its parent)", storeFileName)
	}

	sourceDir, err := resolveSourceDataDir(*from)
	if err != nil {
		return err
	}
	targetRoot, err := cloneTargetRoot(*dataDir)
	if err != nil {
		return err
	}
	targetDir := filepath.Join(targetRoot, appDataDirName)
	if err := refuseUnsafeDataRoot(targetRoot); err != nil {
		return err
	}
	targetRoot, err = instanceinfo.CanonicalPath(targetRoot)
	if err != nil {
		return fmt.Errorf("canonicalize --data-dir: %w", err)
	}
	targetDir = filepath.Join(targetRoot, appDataDirName)
	if err := refuseSecondInstance(e, targetRoot); err != nil {
		return err
	}
	if err := refuseCloneOntoSource(sourceDir, targetDir); err != nil {
		return err
	}

	sourceDB := filepath.Join(sourceDir, storeFileName)
	targetDB := filepath.Join(targetDir, storeFileName)
	if err := prepareCloneTarget(targetRoot, targetDir, targetDB, *force); err != nil {
		return err
	}
	if err := snapshotDatabase(sourceDB, targetDB); err != nil {
		return err
	}
	scrubbed, err := scrubClonedDatabase(e, targetDB)
	if err != nil {
		return err
	}
	attachments, err := copyAttachments(filepath.Join(sourceDir, attachmentsDirName), filepath.Join(targetDir, attachmentsDirName))
	if err != nil {
		return err
	}
	schemaVersion, schemaKnown := readSchemaVersion(targetDB)
	return writeCloneReceipt(e, cloneReceipt{
		sourceDir: sourceDir, targetRoot: targetRoot, targetDir: targetDir,
		sourceDB: sourceDB, targetDB: targetDB, scrubbed: scrubbed,
		attachments: attachments, schemaVersion: schemaVersion, schemaKnown: schemaKnown,
	})
}

type cloneReceipt struct {
	sourceDir, targetRoot, targetDir string
	sourceDB, targetDB               string
	scrubbed                         []scrubResult
	attachments                      attachmentCopy
	schemaVersion                    int64
	schemaKnown                      bool
}

func writeCloneReceipt(e *env, receipt cloneReceipt) error {
	if e.jsonOutput() {
		out := map[string]any{
			"sourceDataDir": receipt.sourceDir,
			"dataRoot":      receipt.targetRoot,
			"dataDir":       receipt.targetDir,
			"database":      receipt.targetDB,
			"scrub":         receipt.scrubbed,
			"attachments":   receipt.attachments,
			"instance":      instanceinfo.ID(receipt.targetRoot),
			"up":            fmt.Sprintf("ao-harness up --data-dir %s", receipt.targetRoot),
		}
		if receipt.schemaKnown {
			out["schemaVersion"] = receipt.schemaVersion
		}
		return e.writeJSON(out)
	}
	e.printf("cloned %s\n", receipt.sourceDB)
	e.printf("     → %s\n", receipt.targetDB)
	if receipt.schemaKnown {
		e.printf("  %-28s v%d (boot migrates forward; a newer store than the binary fails at boot)\n", "schema version", receipt.schemaVersion)
	}
	for _, row := range receipt.scrubbed {
		e.printf("  %-28s %s\n", row.What, row.Detail)
	}
	e.printf("  %-28s %d file(s) copied", "attachments", receipt.attachments.Files)
	if receipt.attachments.Skipped > 0 {
		e.printf(", %d skipped (not a regular file)", receipt.attachments.Skipped)
	}
	e.printf("\n")
	e.printf("  %-28s %s\n", "not copied", strings.Join(cloneExclusions, ", "))
	e.printf("\nboot it:\n  ao-harness up --data-dir %s\n", receipt.targetRoot)
	e.printf("\nthis copy carries your real session content verbatim. It lives only in\n")
	e.printf("%s — never commit it, and delete it when the repro is done.\n", receipt.targetRoot)
	return nil
}

func resolveSourceDataDir(from string) (string, error) {
	abs, err := filepath.Abs(from)
	if err != nil {
		return "", fmt.Errorf("resolve --from %q: %w", from, err)
	}
	candidates := []string{abs, filepath.Join(abs, appDataDirName)}
	for _, candidate := range candidates {
		if info, err := os.Stat(filepath.Join(candidate, storeFileName)); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no %s under --from %s (looked in %s); point --from at the app data dir or the root holding it", storeFileName, abs, strings.Join(candidates, " and "))
}

func cloneTargetRoot(flagValue string) (string, error) {
	if strings.TrimSpace(flagValue) != "" {
		abs, err := filepath.Abs(flagValue)
		if err != nil {
			return "", fmt.Errorf("resolve --data-dir %q: %w", flagValue, err)
		}
		return abs, nil
	}
	return instanceinfo.DefaultDataRoot() + cloneSuffix, nil
}

func refuseCloneOntoSource(sourceDir, targetDir string) error {
	if sameResolvedPath(sourceDir, targetDir) {
		return usagef("clone refuses to write into its own source %s (pick a different --data-dir)", sourceDir)
	}
	if underDir(targetDir, sourceDir) {
		return usagef("clone refuses --data-dir %s: it is inside the source %s", targetDir, sourceDir)
	}
	if underDir(sourceDir, targetDir) {
		return usagef("clone refuses --data-dir %s: the source %s is inside it", targetDir, sourceDir)
	}
	return nil
}

func prepareCloneTarget(targetRoot, targetDir, targetDB string, force bool) error {
	if _, err := os.Stat(targetDB); err == nil {
		if !force {
			return fmt.Errorf("%s already exists; pass --force to replace it, or pick another --data-dir", targetDB)
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(targetDB + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove %s: %w", targetDB+suffix, err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", targetDB, err)
	}
	for _, dir := range []string{targetRoot, targetDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("chmod %s: %w", dir, err)
		}
	}
	return nil
}
