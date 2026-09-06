package git

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/transferfiles"
)

// transferBlobSizes validates the expanded checkout before creating files. Pack
// size is not an expansion limit: one tiny compressed blob can be referenced by
// many paths. Count each index entry, not just each distinct object.
func (c *Core) transferBlobSizes(ctx context.Context, cwd string, entries []TransferIndexEntry, remaining int64) ([]int64, error) {
	if err := validateTransferIndex(entries); err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	var input strings.Builder
	for _, entry := range entries {
		input.WriteString(entry.OID)
		input.WriteByte('\n')
	}
	output, _, err := c.executeSpec(commandSpec{binary: "git", cwd: cwd, ctx: ctx, stdin: input.String(),
		args:     []string{"cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)"},
		extraEnv: []string{"GIT_NO_LAZY_FETCH=1"}, maxBytes: maxTransferIndexBytes, timeout: 2 * time.Minute})
	if err != nil {
		return nil, err
	}
	sizes := make([]int64, 0, len(entries))
	for line := range strings.SplitSeq(strings.TrimSuffix(output, "\n"), "\n") {
		fields := strings.Fields(line)
		if len(sizes) >= len(entries) || len(fields) != 3 || fields[0] != entries[len(sizes)].OID || fields[1] != "blob" {
			return nil, errors.New("The transferred workspace references a missing or invalid Git blob.")
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 || size > transferfiles.MaxFileBytes || size > remaining {
			return nil, errors.New("The expanded workspace exceeds the transfer size limit.")
		}
		if entries[len(sizes)].Mode == "120000" && size > 4096 {
			return nil, errors.New("The workspace contains an oversized symbolic link.")
		}
		remaining -= size
		sizes = append(sizes, size)
	}
	if len(sizes) != len(entries) {
		return nil, fmt.Errorf("transfer: Git returned %d of %d blob sizes", len(sizes), len(entries))
	}
	return sizes, nil
}
