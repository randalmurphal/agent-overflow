//go:build linux

package hostmemory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseMeminfo(t *testing.T) {
	meminfo := []byte("MemTotal:       53457840 kB\nMemFree:         8651028 kB\nMemAvailable:   47887028 kB\nBuffers: 1 kB\n")
	for key, want := range map[string]uint64{"MemAvailable": 47887028 << 10, "MemTotal": 53457840 << 10} {
		if got, err := parseMeminfo(meminfo, key); err != nil || got != want {
			t.Fatalf("parseMeminfo(%s) = %d, %v; want %d", key, got, err, want)
		}
	}
	for _, bad := range []string{"MemTotal: 1 kB\n", "MemAvailable: lots kB\n", "MemAvailable: 12 MB\n"} {
		if _, err := parseMeminfo([]byte(bad), "MemAvailable"); err == nil {
			t.Fatalf("parseMeminfo(%q) accepted", bad)
		}
	}
	// The host's own files parse.
	if r, err := Read(); err != nil || r.Total == 0 || r.Available == 0 || r.Headroom() == 0 {
		t.Fatalf("Read = %+v, %v", r, err)
	}
}

// writeTree writes files, path to content, under a new directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, value := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestCgroupMemory: the lowest limit on the cgroup's path counts, v1 when
// its memory controller is mounted, else v2; "max", a missing file and no
// cgroup at all set none, and an unreadable value is an error. The
// headroom is the least, over the levels with a limit, of the limit less
// the usage that inactive file pages do not cover.
func TestCgroupMemory(t *testing.T) {
	const gib = 1 << 30
	root := writeTree(t, map[string]string{
		"user.slice/memory.max":                   "17179869184",
		"user.slice/memory.current":               "15032385536",
		"user.slice/memory.stat":                  "active_file 1\ninactive_file 1073741824",
		"user.slice/session/memory.max":           "max",
		"user.slice/session/app/memory.max":       "34359738368",
		"user.slice/session/app/memory.current":   "1073741824",
		"user.slice/session/app/memory.stat":      "inactive_file 0",
		"memory/docker/abc/memory.limit_in_bytes": "8589934592",
		"memory/docker/abc/memory.usage_in_bytes": "9663676416",
		"memory/docker/abc/memory.stat":           "inactive_file 5\ntotal_inactive_file 2147483648",
		"memory/docker/memory.limit_in_bytes":     "9223372036854771712",
		"memory/docker/memory.usage_in_bytes":     "9663676416",
		"memory/docker/memory.stat":               "total_inactive_file 0",
		"broken/memory.max":                       "lots",
		"nousage/memory.max":                      "1073741824",
		"overcharged/memory.max":                  "1073741824",
		"overcharged/memory.current":              "2147483648",
		"overcharged/memory.stat":                 "inactive_file 0",
	})
	for _, tc := range []struct {
		name, cgroup    string
		limit, headroom uint64
		fails           bool
	}{
		{name: "v2 ancestor lowest", cgroup: "0::/user.slice/session/app\n", limit: 16 * gib, headroom: 3 * gib},
		{name: "v2 none set", cgroup: "0::/other/leaf\n"},
		{name: "v2 root", cgroup: "0::/\n"},
		{name: "v1 memory controller", cgroup: "12:pids:/docker/abc\n4:cpu,memory:/docker/abc\n0::/\n", limit: 8 * gib, headroom: 1 * gib},
		{name: "v1 unlimited", cgroup: "4:memory:/docker\n", limit: 9223372036854771712, headroom: 9223372036854771712 - 9*gib},
		{name: "over its limit", cgroup: "0::/overcharged\n", limit: 1 * gib, headroom: 0},
		{name: "no cgroup", cgroup: ""},
		{name: "unreadable value", cgroup: "0::/broken\n", fails: true},
		{name: "a limit without usage", cgroup: "0::/nousage\n", fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limit, headroom, err := cgroupMemory([]byte(tc.cgroup), root)
			if (err != nil) != tc.fails || limit != tc.limit || headroom != tc.headroom {
				t.Fatalf("cgroupMemory = %d, %d, %v; want %d, %d, failure %v", limit, headroom, err, tc.limit, tc.headroom, tc.fails)
			}
		})
	}
}

// TestHeadroomIsTheLowerOfTheHostAndTheCgroup reads a host with 64 GiB
// available, in a cgroup with 3 GiB of room, and in one with none set.
func TestHeadroomIsTheLowerOfTheHostAndTheCgroup(t *testing.T) {
	const gib = 1 << 30
	root := writeTree(t, map[string]string{
		"proc/meminfo":              "MemTotal: 134217728 kB\nMemAvailable: 67108864 kB",
		"proc/self/cgroup":          "0::/job",
		"cgroup/job/memory.max":     "4294967296",
		"cgroup/job/memory.current": "1073741824",
		"cgroup/job/memory.stat":    "inactive_file 0",
	})
	r, err := ReadAt(filepath.Join(root, "proc"), filepath.Join(root, "cgroup"))
	if err != nil || r.Available != 64*gib || r.Total != 128*gib || r.CgroupLimit != 4*gib || r.Headroom() != 3*gib {
		t.Fatalf("ReadAt = %+v (headroom %d), %v; want 64 GiB available, a 4 GiB limit and 3 GiB of headroom", r, r.Headroom(), err)
	}
	if r := (Reading{Available: 2 * gib, CgroupLimit: 4 * gib, CgroupHeadroom: 3 * gib}); r.Headroom() != 2*gib {
		t.Fatalf("headroom %d with the host lower; want the host's", r.Headroom())
	}
	if r := (Reading{Available: 2 * gib}); r.Headroom() != 2*gib {
		t.Fatalf("headroom %d with no limit; want the host's", r.Headroom())
	}
}
