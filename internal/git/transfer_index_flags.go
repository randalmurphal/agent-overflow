package git

import (
	"context"
	"errors"
	"slices"
	"strings"
)

// Read semantic flags without touching working files or running checkout filters.
// The binary index also contains expendable stat/cache data, so hashing its bytes
// would reject an innocent git status between preparation and activation.
func (c *Core) readTransferIndexFlags(ctx context.Context, cwd string) ([]TransferIndexFlags, error) {
	flagOutput, _, err := c.executeSpec(commandSpec{binary: "git", cwd: cwd, ctx: ctx, args: []string{"ls-files", "-v", "-z"}, maxBytes: maxTransferIndexBytes})
	if err != nil {
		return nil, err
	}
	flags := make(map[string]TransferIndexFlags)
	for row := range strings.SplitSeq(flagOutput, "\x00") {
		if row == "" {
			continue
		}
		if len(row) < 3 || row[1] != ' ' || !transferWorkspacePath(row[2:]) {
			return nil, errors.New("transfer: invalid index flags")
		}
		switch row[0] {
		case 'H':
		case 'h', 'S', 's':
			flags[row[2:]] = TransferIndexFlags{Path: row[2:], SkipWorktree: row[0] == 'S' || row[0] == 's', AssumeUnchanged: row[0] == 'h' || row[0] == 's'}
		default:
			return nil, errors.New("transfer: unsupported index flags")
		}
	}
	visible, err := c.transferPathList(ctx, cwd, "diff", "--cached", "--no-ext-diff", "--no-textconv", "--name-only", "-z", "--diff-filter=A", "--ita-visible-in-index")
	if err != nil {
		return nil, err
	}
	invisible, err := c.transferPathList(ctx, cwd, "diff", "--cached", "--no-ext-diff", "--no-textconv", "--name-only", "-z", "--diff-filter=A", "--ita-invisible-in-index")
	if err != nil {
		return nil, err
	}
	staged := make(map[string]bool, len(invisible))
	for _, path := range invisible {
		staged[path] = true
	}
	for _, path := range visible {
		if !staged[path] {
			flag := flags[path]
			flag.Path, flag.IntentToAdd = path, true
			flags[path] = flag
		}
	}

	result := make([]TransferIndexFlags, 0, len(flags))
	for _, flag := range flags {
		result = append(result, flag)
	}
	slices.SortFunc(result, func(a, b TransferIndexFlags) int { return strings.Compare(a.Path, b.Path) })
	return result, nil
}
