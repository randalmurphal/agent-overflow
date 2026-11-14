package git

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// GitTag is a tag the branch picker can offer. Date is git's creatordate in
// unix seconds: the tagger date of an annotated tag, the commit date of a
// lightweight one.
type GitTag struct {
	Name string `json:"name"`
	Date int64  `json:"date"`
}

// tagListMaxBytes bounds the tag listing. A record is a name and a date,
// so this holds several hundred thousand tags; past it the listing fails
// rather than answering with a silent prefix.
const tagListMaxBytes = 16 << 20

// ErrNoOriginRemote is a tag refresh in a repository with no origin.
var ErrNoOriginRemote = errors.New("this repository has no origin remote")

// ListTags returns every tag, newest first: creatordate descending, then
// name.
func (c *Core) ListTags(cwd string) ([]GitTag, error) {
	result, err := c.runSpec(commandSpec{
		binary:   "git",
		cwd:      cwd,
		maxBytes: tagListMaxBytes,
		args: []string{"for-each-ref", "--sort=refname", "--sort=-creatordate",
			"--format=%(refname:lstrip=2)%00%(creatordate:unix)", "refs/tags"},
	})
	if err != nil {
		return nil, err
	}
	if result.exitCode != 0 {
		return nil, fmt.Errorf("git for-each-ref refs/tags failed: %s", strings.TrimSpace(result.stderr))
	}
	return parseTagList(result.stdout), nil
}

// parseTagList reads ListTags' records: name NUL date, one per line. Ref
// names cannot hold a newline or NUL.
func parseTagList(stdout string) []GitTag {
	tags := []GitTag{}
	for _, line := range strings.Split(stdout, "\n") {
		name, rawDate, ok := strings.Cut(line, "\x00")
		if !ok || name == "" {
			continue
		}
		date, _ := strconv.ParseInt(strings.TrimSpace(rawDate), 10, 64)
		tags = append(tags, GitTag{Name: name, Date: date})
	}
	return tags
}

// RefreshTags fetches origin's tags on behalf of a person who asked for it,
// so a credential prompt may appear. --force is required: without it one
// tag moved upstream fails the whole fetch and stores no tag at all. Tags
// are never pruned, so a tag only this repository has survives: --no-prune
// overrides fetch.prune and remote.origin.prune, without which a configured
// pruneTags deletes nothing, and --no-prune-tags overrides pruneTags too.
func (c *Core) RefreshTags(cwd string) error {
	if !c.originRemoteExists(cwd) {
		return ErrNoOriginRemote
	}
	if _, _, err := c.executeInteractive(cwd, "fetch", "--no-prune", "--no-prune-tags", "--tags", "--force", "origin"); err != nil {
		return fmt.Errorf("refresh tags from origin: %w", err)
	}
	return nil
}
