package instanceinfo

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agent-overflow/internal/atomicfile"
)

// RegistryDir is where rows live: <user cache dir>/agent-overflow/
// harness-instances. Registry files are derived discovery state. Losing one
// costs a listing, never instance data.
func RegistryDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("instanceinfo: resolve user cache dir: %w", err)
	}
	return filepath.Join(base, "agent-overflow", "harness-instances"), nil
}

func WriteIn(dir string, row Row) error {
	path, err := rowPath(dir, row.ID)
	if err != nil {
		return err
	}
	if err := atomicfile.WriteJSON(path, row); err != nil {
		return fmt.Errorf("instanceinfo: write %s: %w", path, err)
	}
	return nil
}

func RemoveIn(dir, id string) error {
	path, err := rowPath(dir, id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("instanceinfo: remove %s: %w", path, err)
	}
	return nil
}

// ListIn reads registry state with an injectable liveness probe. Malformed
// rows are skipped because one corrupt discovery file must not hide healthy
// instances.
func ListIn(dir string, alive func(pid int) bool) ([]Instance, error) {
	if alive == nil {
		alive = ProcessAlive
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("instanceinfo: read registry %s: %w", dir, err)
	}
	out := make([]Instance, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var row Row
		if err := json.Unmarshal(data, &row); err != nil || row.ID == "" {
			continue
		}
		rowAlive := alive(row.PID)
		if row.PIDNamespace != "" && row.PIDNamespace != CurrentPIDNamespace() {
			rowAlive = true
		}
		out = append(out, Instance{Row: row, Path: path, Stale: !rowAlive})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	return out, nil
}

func rowPath(dir, id string) (string, error) {
	if dir == "" {
		return "", errors.New("instanceinfo: empty registry dir")
	}
	if !ValidID(id) {
		return "", fmt.Errorf("instanceinfo: %q is not an instance id (want %d lowercase hex chars)", id, idHexLen)
	}
	return filepath.Join(dir, id+".json"), nil
}

func ValidID(id string) bool {
	if len(id) != idHexLen {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
