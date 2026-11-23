//go:build linux

package hostmemory

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Reading is what /proc and the cgroup file system say of the memory this
// process may use, in bytes.
type Reading struct {
	// Available and Total are /proc/meminfo's MemAvailable and MemTotal.
	Available, Total uint64
	// CgroupLimit is the lowest memory limit set on this process's cgroup
	// or any cgroup above it, or 0 when none is set.
	CgroupLimit uint64
	// CgroupHeadroom is, over those cgroups with a limit, the least room
	// left below it: the limit less what the cgroup is charged and cannot
	// reclaim, its usage less its inactive file pages. It is 0 when no
	// limit is set.
	CgroupHeadroom uint64
}

// Headroom is how much more this process can allocate: the host's
// MemAvailable, or its cgroups' headroom when that is lower.
func (r Reading) Headroom() uint64 {
	if r.CgroupLimit != 0 && r.CgroupHeadroom < r.Available {
		return r.CgroupHeadroom
	}
	return r.Available
}

// Read reads the host's /proc and its cgroup file system.
func Read() (Reading, error) { return ReadAt("/proc", "/sys/fs/cgroup") }

// ReadAt reads as Read does, with /proc at proc and the cgroup file system
// mounted at cgroupRoot.
func ReadAt(proc, cgroupRoot string) (Reading, error) {
	meminfo, err := os.ReadFile(filepath.Join(proc, "meminfo"))
	if err != nil {
		return Reading{}, err
	}
	var r Reading
	if r.Available, err = parseMeminfo(meminfo, "MemAvailable"); err != nil {
		return Reading{}, err
	}
	if r.Total, err = parseMeminfo(meminfo, "MemTotal"); err != nil {
		return Reading{}, err
	}
	cgroups, err := os.ReadFile(filepath.Join(proc, "self", "cgroup"))
	if err != nil {
		return Reading{}, fmt.Errorf("read this process's cgroup: %w", err)
	}
	if r.CgroupLimit, r.CgroupHeadroom, err = cgroupMemory(cgroups, cgroupRoot); err != nil {
		return Reading{}, err
	}
	return r, nil
}

// parseMeminfo reads the line of /proc/meminfo for key, in bytes.
func parseMeminfo(meminfo []byte, key string) (uint64, error) {
	scanner := bufio.NewScanner(bytes.NewReader(meminfo))
	for scanner.Scan() {
		value, ok := strings.CutPrefix(scanner.Text(), key+":")
		if !ok {
			continue
		}
		kB, ok := strings.CutSuffix(strings.TrimSpace(value), " kB")
		if !ok {
			return 0, fmt.Errorf("%s %q is not in kB", key, strings.TrimSpace(value))
		}
		n, err := strconv.ParseUint(kB, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", key, err)
		}
		if n > ^uint64(0)>>10 {
			return 0, fmt.Errorf("%s overflows bytes", key)
		}
		return n << 10, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("no %s in /proc/meminfo", key)
}

// cgroupVersion names one cgroup version's memory files.
type cgroupVersion struct {
	limit, usage, inactive string
}

var (
	cgroupV1 = cgroupVersion{limit: "memory.limit_in_bytes", usage: "memory.usage_in_bytes", inactive: "total_inactive_file"}
	cgroupV2 = cgroupVersion{limit: "memory.max", usage: "memory.current", inactive: "inactive_file"}
)

// cgroupMemory returns the lowest memory limit set on the cgroup
// procCgroup (/proc/self/cgroup) names or any of its ancestors, under the
// cgroup file system mounted at root, and the least headroom below such a
// limit, or 0 and 0 when none is set: the v1 memory controller's files
// when that controller is mounted, else v2's. "max", and the v1 near-2^63
// "unlimited", mean none; a limit at or above MemTotal limits nothing. A
// level without the limit file, as the root has, sets none.
func cgroupMemory(procCgroup []byte, root string) (limit, headroom uint64, err error) {
	var v1, v2 string
	haveV1, haveV2 := false, false
	for _, line := range strings.Split(string(procCgroup), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 {
			continue
		}
		if fields[0] == "0" && fields[1] == "" {
			v2, haveV2 = fields[2], true
		}
		for _, controller := range strings.Split(fields[1], ",") {
			if controller == "memory" {
				v1, haveV1 = fields[2], true
			}
		}
	}
	var base, cgroup string
	var files cgroupVersion
	switch {
	case haveV1:
		base, cgroup, files = filepath.Join(root, "memory"), v1, cgroupV1
	case haveV2:
		base, cgroup, files = root, v2, cgroupV2
	default:
		return 0, 0, nil
	}
	found := false
	for dir := path.Clean("/" + cgroup); ; dir = path.Dir(dir) {
		level := filepath.Join(base, dir)
		n, set, err := readLimit(filepath.Join(level, files.limit))
		if err != nil {
			return 0, 0, err
		}
		if set {
			room, err := levelHeadroom(level, files, n)
			if err != nil {
				return 0, 0, err
			}
			if !found || n < limit {
				limit = n
			}
			if !found || room < headroom {
				headroom = room
			}
			found = true
		}
		if dir == "/" {
			return limit, headroom, nil
		}
	}
}

// readLimit reads a level's limit file: set is false when the level has
// none, or "max".
func readLimit(file string) (n uint64, set bool, err error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read the cgroup memory limit: %w", err)
	}
	value := strings.TrimSpace(string(data))
	if value == "max" {
		return 0, false, nil
	}
	n, err = strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("cgroup memory limit %s: %w", file, err)
	}
	// A limit of 0 is set, unlike CgroupLimit's 0; one byte limits the
	// same.
	return max(n, 1), true, nil
}

// levelHeadroom is what a level with limit can still be charged: the limit
// less its usage, its inactive file pages counted as free, as the kernel
// reclaims them before it fails a charge.
func levelHeadroom(level string, files cgroupVersion, limit uint64) (uint64, error) {
	data, err := os.ReadFile(filepath.Join(level, files.usage))
	if err != nil {
		return 0, fmt.Errorf("read the cgroup memory usage: %w", err)
	}
	usage, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cgroup memory usage %s: %w", filepath.Join(level, files.usage), err)
	}
	stat, err := os.ReadFile(filepath.Join(level, "memory.stat"))
	if err != nil {
		return 0, fmt.Errorf("read the cgroup memory statistics: %w", err)
	}
	var inactive uint64
	for line := range strings.Lines(string(stat)) {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), files.inactive+" ")
		if !ok {
			continue
		}
		if inactive, err = strconv.ParseUint(value, 10, 64); err != nil {
			return 0, fmt.Errorf("cgroup %s %s: %w", files.inactive, filepath.Join(level, "memory.stat"), err)
		}
		break
	}
	charged := usage - min(usage, inactive)
	return limit - min(limit, charged), nil
}
