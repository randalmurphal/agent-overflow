package git

import (
	"fmt"
	"strconv"
	"strings"
)

// GitBranch is a branch the picker can offer for checkout. Remote-only
// branches are projected to their short name (e.g. "feature" rather than
// "origin/feature") so the UI presents a unified list; `git checkout <name>`
// against a name that exists only on a remote auto-creates the local tracking
// branch.
//
// AheadCount and BehindCount are populated from %(upstream:track) when the
// branch has a configured upstream. Remote-only projected entries (those
// without a local checkout) have no upstream relationship, so both counts stay
// 0.
type GitBranch struct {
	Name         string `json:"name"`
	IsCurrent    bool   `json:"isCurrent"`
	IsDefault    bool   `json:"isDefault"`
	WorktreePath string `json:"worktreePath,omitempty"`
	AheadCount   int    `json:"aheadCount,omitempty"`
	BehindCount  int    `json:"behindCount,omitempty"`
}

// BranchIsDefault reports whether branch is the repo's default (e.g.
// `main`/`master`). Returns false when ListBranches fails or when the
// branch isn't present in the listing.
func (c *Core) BranchIsDefault(cwd, branch string) bool {
	branches, err := c.ListBranches(cwd)
	if err != nil {
		return false
	}
	for _, candidate := range branches {
		if candidate.Name == branch {
			return candidate.IsDefault
		}
	}
	return false
}

// ListBranches returns local and remote branches from git branch output.
func (c *Core) ListBranches(cwd string) ([]GitBranch, error) {
	result, err := c.run(cwd, "branch", "-a", "--sort=-committerdate",
		"--format=%(refname:short)|%(HEAD)|%(worktreepath)|%(upstream:track,nobracket)")
	if err != nil {
		return nil, err
	}
	if result.exitCode != 0 {
		return nil, fmt.Errorf("git branch failed: %s", strings.TrimSpace(result.stderr))
	}

	defaultBranch, _ := c.defaultBranchName(cwd)
	return parseBranchList(result.stdout, defaultBranch, c.listRemoteNames(cwd)), nil
}

func parseBranchList(stdout, defaultBranch string, remoteNames []string) []GitBranch {
	type rawEntry struct {
		name         string
		isCurrent    bool
		worktreePath string
		ahead        int
		behind       int
		isRemote     bool
	}

	remoteSet := make(map[string]struct{}, len(remoteNames))
	for _, r := range remoteNames {
		remoteSet[r] = struct{}{}
	}

	var raws []rawEntry
	localSet := make(map[string]struct{})
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 4)
		if len(parts) != 4 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		if name == "" {
			continue
		}
		// Drop remote symrefs. Two forms appear in the wild:
		//   - `refs/remotes/<r>/HEAD` -> "<r>/HEAD" (standard refname:short)
		//   - same ref but git collapses to the bare "<r>" on some versions.
		// Both must go; a local branch literally named "<r>/HEAD" or "<r>"
		// would be filtered too, but those are degenerate names nobody picks
		// intentionally.
		if _, isBareRemote := remoteSet[name]; isBareRemote {
			continue
		}
		isRemote := isRemoteBranchName(name, remoteNames)
		if isRemote && strings.HasSuffix(name, "/HEAD") {
			continue
		}
		ahead, behind := parseUpstreamTrack(parts[3])
		raws = append(raws, rawEntry{
			name:         name,
			isCurrent:    strings.TrimSpace(parts[1]) == "*",
			worktreePath: strings.TrimSpace(parts[2]),
			ahead:        ahead,
			behind:       behind,
			isRemote:     isRemote,
		})
		if !isRemote {
			localSet[name] = struct{}{}
		}
	}

	seenRemoteBase := make(map[string]struct{})
	var branches []GitBranch
	for _, e := range raws {
		name := e.name
		if e.isRemote {
			base := stripRemotePrefix(name, remoteNames)
			if _, hasLocal := localSet[base]; hasLocal {
				continue
			}
			if _, seen := seenRemoteBase[base]; seen {
				continue
			}
			seenRemoteBase[base] = struct{}{}
			name = base
		}
		branches = append(branches, GitBranch{
			Name:         name,
			IsCurrent:    e.isCurrent,
			IsDefault:    isDefaultBranchName(name, defaultBranch),
			WorktreePath: e.worktreePath,
			AheadCount:   e.ahead,
			BehindCount:  e.behind,
		})
	}
	return branches
}

// parseUpstreamTrack reads the output of `--format=%(upstream:track,nobracket)`.
// Examples: "" (no upstream or in sync), "gone" (upstream deleted), "ahead 3",
// "behind 2", "ahead 3, behind 2". Unknown words contribute 0.
func parseUpstreamTrack(raw string) (ahead, behind int) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "gone" {
		return 0, 0
	}
	for _, part := range strings.Split(raw, ",") {
		fields := strings.Fields(part)
		if len(fields) != 2 {
			continue
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		switch fields[0] {
		case "ahead":
			ahead = n
		case "behind":
			behind = n
		}
	}
	return ahead, behind
}

func stripRemotePrefix(name string, remoteNames []string) string {
	for _, r := range remoteNames {
		if strings.HasPrefix(name, r+"/") {
			return strings.TrimPrefix(name, r+"/")
		}
	}
	return name
}

func isDefaultBranchName(branchName, defaultBranch string) bool {
	if branchName == "" {
		return false
	}

	if defaultBranch == "" {
		return branchName == "main" || branchName == "master" ||
			strings.HasSuffix(branchName, "/main") || strings.HasSuffix(branchName, "/master")
	}

	if branchName == defaultBranch {
		return true
	}
	return strings.HasSuffix(branchName, "/"+defaultBranch)
}

func (c *Core) defaultBranchName(cwd string) (string, error) {
	result, err := c.run(cwd, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", err
	}
	if result.exitCode != 0 {
		return "", nil
	}
	return strings.TrimPrefix(strings.TrimSpace(result.stdout), "refs/remotes/origin/"), nil
}

func (c *Core) originRemoteExists(cwd string) bool {
	return c.originRemote(cwd).known
}

func (c *Core) listRemoteNames(cwd string) []string {
	result, err := c.run(cwd, "remote")
	if err != nil || result.exitCode != 0 {
		return nil
	}

	var names []string
	for _, line := range strings.Split(result.stdout, "\n") {
		name := strings.TrimSpace(line)
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func isRemoteBranchName(name string, remoteNames []string) bool {
	for _, remote := range remoteNames {
		if strings.HasPrefix(name, remote+"/") {
			return true
		}
	}
	return false
}
