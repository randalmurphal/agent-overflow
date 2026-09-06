package rollout

import (
	"context"
	"fmt"

	"agent-overflow/internal/transferfiles"
)

// TransferMinimumVersion derives compatibility from the native format, not the
// source computer's installed version. Standalone paginated exports produced by
// 0.153.4 were resumed, continued and reverted with 0.148.0 in isolated probes.
// Legacy files use AO's ordinary provider floor. Unknown modes remain refusals.
func TransferMinimumVersion(ctx context.Context, files []transferfiles.Source) (string, error) {
	if len(files) > transferfiles.MaxFiles {
		return "", fmt.Errorf("codex transfer: too many native files")
	}
	minimum := ""
	for _, file := range files {
		meta, err := readTransferMeta(ctx, file, "")
		if err != nil {
			return "", err
		}
		switch meta.HistoryMode {
		case "", HistoryModeLegacy:
		case HistoryModePaginated:
			minimum = "0.148.0"
		default:
			return "", fmt.Errorf("codex transfer: unsupported history mode %q", meta.HistoryMode)
		}
	}
	return minimum, nil
}
