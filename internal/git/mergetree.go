package git

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var treeOIDPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// maxPRDiffBytes caps a locally-computed (or API-fetched) PR diff. gh/glab's
// PR-diff endpoints refuse diffs over 20k lines (HTTP 406); the local path
// has no such limit, so this bounds memory instead. Matches the checkpoint
// store's diff ceiling.
const maxPRDiffBytes = 10 * 1024 * 1024

// DiffMergeBase returns the unified diff from the merge base of base and
// head to head — the three-dot PR diff. This is the same content gh/glab's
// PR-diff endpoints return, computed from local objects so it isn't subject
// to their 20k-line API cap. Both refs must already exist locally (fetch
// the PR head ref and the base branch first).
func (c *Core) DiffMergeBase(cwd, base, head string) (string, error) {
	base = strings.TrimSpace(base)
	head = strings.TrimSpace(head)
	if base == "" {
		return "", fmt.Errorf("git diff base is required")
	}
	if head == "" {
		return "", fmt.Errorf("git diff head is required")
	}
	// Trailing `--` so a ref that resembles a flag can't inject options.
	result, err := c.runWithLimit(cwd, maxPRDiffBytes,
		"diff", "--merge-base", "--no-color", "--no-ext-diff", "--no-textconv",
		base, head, "--")
	if err != nil {
		return "", err
	}
	if result.exitCode != 0 {
		message := strings.TrimSpace(result.stderr)
		if message == "" {
			message = fmt.Sprintf("git diff exit code %d", result.exitCode)
		}
		return "", fmt.Errorf("git diff failed: %s", message)
	}
	return result.stdout, nil
}

type MergeTreeResult struct {
	Conflicted bool     `json:"conflicted"`
	TreeOID    string   `json:"treeOID"`
	Paths      []string `json:"paths"`
	// Notes maps a conflicted path to the merge-tree messages describing
	// it — the only signal for conflicts with no renderable content
	// (modify/delete, rename/rename, file/directory, …). Redundant
	// "CONFLICT (content)" and "Auto-merging" lines are dropped: those
	// files render with visible conflict regions and a per-file badge.
	Notes map[string][]string `json:"notes"`
	// Messages holds the leftover messages that mention no conflicted
	// path (rare); the frontend renders them as a fallback strip.
	Messages []string `json:"messages"`
}

func (c *Core) MergeTreeConflicts(cwd, base, head string) (MergeTreeResult, error) {
	base = strings.TrimSpace(base)
	head = strings.TrimSpace(head)
	if base == "" {
		return MergeTreeResult{}, fmt.Errorf("git merge-tree base is required")
	}
	if head == "" {
		return MergeTreeResult{}, fmt.Errorf("git merge-tree head is required")
	}

	result, err := c.run(cwd, "merge-tree", "--write-tree", "--name-only", base, head)
	if err != nil {
		return MergeTreeResult{}, err
	}
	switch result.exitCode {
	case 0:
		parsed, err := parseMergeTreeNameOnly(result.stdout, false)
		if err != nil {
			return MergeTreeResult{}, err
		}
		return parsed, nil
	case 1:
		parsed, err := parseMergeTreeNameOnly(result.stdout, true)
		if err != nil {
			return MergeTreeResult{}, err
		}
		return parsed, nil
	default:
		message := strings.TrimSpace(result.stderr)
		if message == "" {
			message = strings.TrimSpace(result.stdout)
		}
		if message == "" {
			message = fmt.Sprintf("exit code %d", result.exitCode)
		}
		return MergeTreeResult{}, fmt.Errorf("git merge-tree failed: %s", message)
	}
}

func (c *Core) FetchRefOID(cwd, remote, ref string) (string, error) {
	remote = strings.TrimSpace(remote)
	ref = strings.TrimSpace(ref)
	if err := validateFetchArg("git fetch remote", remote); err != nil {
		return "", err
	}
	if err := validateFetchArg("git fetch ref", ref); err != nil {
		return "", err
	}
	if _, _, err := c.Execute(cwd, "fetch", remote, ref); err != nil {
		return "", err
	}
	// FETCH_HEAD is overwritten by the next fetch, so capture the OID
	// immediately before fetching anything else.
	stdout, _, err := c.Execute(cwd, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

func (c *Core) FetchBranch(cwd, remote, branch string) error {
	remote = strings.TrimSpace(remote)
	branch = strings.TrimSpace(branch)
	if err := validateFetchArg("git fetch remote", remote); err != nil {
		return err
	}
	if branch == "" {
		return fmt.Errorf("git fetch branch is required")
	}
	if err := validateBranchName(branch); err != nil {
		return err
	}
	_, _, err := c.Execute(cwd, "fetch", remote, branch)
	return err
}

func (c *Core) ShowTreeFile(cwd, treeOID, path string) (string, error) {
	treeOID = strings.TrimSpace(treeOID)
	if !treeOIDPattern.MatchString(treeOID) {
		return "", fmt.Errorf("git show tree OID must be a 40-64 character lowercase hex object ID")
	}
	if path == "" {
		return "", fmt.Errorf("git show path is required")
	}
	if strings.ContainsRune(path, '\x00') {
		return "", fmt.Errorf("git show path must not contain NUL")
	}
	if strings.HasPrefix(path, "-") {
		return "", fmt.Errorf("git show path must not start with '-'")
	}
	stdout, _, err := c.Execute(cwd, "show", treeOID+":"+path)
	if err != nil {
		return "", err
	}
	return stdout, nil
}

func PRHeadRef(forgeID string, number int) (string, error) {
	if number <= 0 {
		return "", fmt.Errorf("PR number must be positive, got %d", number)
	}
	switch strings.TrimSpace(strings.ToLower(forgeID)) {
	case "github":
		return "pull/" + strconv.Itoa(number) + "/head", nil
	case "gitlab":
		return "merge-requests/" + strconv.Itoa(number) + "/head", nil
	default:
		return "", fmt.Errorf("unsupported forge %q", forgeID)
	}
}

func parseMergeTreeNameOnly(stdout string, conflicted bool) (MergeTreeResult, error) {
	lines := strings.Split(strings.ReplaceAll(stdout, "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return MergeTreeResult{}, fmt.Errorf("git merge-tree output missing tree OID")
	}
	out := MergeTreeResult{
		Conflicted: conflicted,
		TreeOID:    strings.TrimSpace(lines[0]),
	}
	if !treeOIDPattern.MatchString(out.TreeOID) {
		return MergeTreeResult{}, fmt.Errorf("git merge-tree output has invalid tree OID %q", out.TreeOID)
	}
	if !conflicted {
		return out, nil
	}

	inMessages := false
	var messages []string
	for _, raw := range lines[1:] {
		line := strings.TrimSuffix(raw, "\r")
		if !inMessages && line == "" {
			inMessages = true
			continue
		}
		if inMessages {
			if strings.TrimSpace(line) != "" && !isRedundantMergeMessage(line) {
				messages = append(messages, line)
			}
			continue
		}
		out.Paths = append(out.Paths, line)
	}
	out.Notes, out.Messages = attributeMergeMessages(messages, out.Paths)
	return out, nil
}

// isRedundantMergeMessage reports messages the conflict viewer already
// expresses on its own: "Auto-merging <path>" progress noise and
// "CONFLICT (content)" lines, whose files render with visible conflict
// regions and a conflict-count badge.
func isRedundantMergeMessage(line string) bool {
	return strings.HasPrefix(line, "Auto-merging ") ||
		strings.HasPrefix(line, "CONFLICT (content): ")
}

// attributeMergeMessages assigns each message to the conflicted path it
// mentions (longest match wins so nested paths attribute correctly);
// messages naming no conflicted path are returned separately.
func attributeMergeMessages(messages, paths []string) (map[string][]string, []string) {
	var notes map[string][]string
	var rest []string
	for _, message := range messages {
		best := ""
		for _, path := range paths {
			if len(path) > len(best) && strings.Contains(message, path) {
				best = path
			}
		}
		if best == "" {
			rest = append(rest, message)
			continue
		}
		if notes == nil {
			notes = make(map[string][]string)
		}
		notes[best] = append(notes[best], message)
	}
	return notes, rest
}

func validateFetchArg(label, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", label)
	}
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s must not contain NUL", label)
	}
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("%s must not start with '-'", label)
	}
	return nil
}
