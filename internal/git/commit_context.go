package git

import (
	"strconv"
	"strings"
)

// RecentCommitSubjects returns the subjects of the newest n non-merge
// commits at cwd, newest first. Best-effort: any failure (not a repo,
// no commits yet) returns nil — callers use the result as style
// guidance for generated commit messages, never as data.
func (c *Core) RecentCommitSubjects(cwd string, n int) []string {
	if n <= 0 {
		return nil
	}
	result, err := c.run(cwd, "log", "-n", strconv.Itoa(n), "--no-merges", "--pretty=format:%s")
	if err != nil || result.exitCode != 0 {
		return nil
	}
	var subjects []string
	for line := range strings.SplitSeq(result.stdout, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			subjects = append(subjects, line)
		}
	}
	return subjects
}
