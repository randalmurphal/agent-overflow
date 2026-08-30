//go:build darwin && cgo

package procrss

import (
	"errors"
	"os"
	"reflect"
	"testing"
)

type fakeDarwinReader struct {
	rows     []darwinProcess
	owners   map[int]int
	details  map[int]Process
	ownerErr error
}

func (f fakeDarwinReader) processes() ([]darwinProcess, error) { return f.rows, nil }
func (f fakeDarwinReader) responsible(pid int) (int, error) {
	if f.ownerErr != nil {
		return 0, f.ownerErr
	}
	return f.owners[pid], nil
}
func (f fakeDarwinReader) process(pid int, fallback string) (Process, error) {
	proc, ok := f.details[pid]
	if !ok {
		return Process{}, errors.New("gone")
	}
	if proc.Name == "" {
		proc.Name = fallback
	}
	return proc, nil
}

func TestSampleDarwinIncludesLaunchdParentedResponsibleWebKit(t *testing.T) {
	reader := fakeDarwinReader{
		rows: []darwinProcess{
			{pid: 1, ppid: 0, shortName: "launchd"},
			{pid: 100, ppid: 1, shortName: "agent-overflow"},
			{pid: 101, ppid: 100, shortName: "provider"},
			{pid: 200, ppid: 1, shortName: "com.apple.WebKit"},
			{pid: 300, ppid: 1, shortName: "com.apple.WebKit"},
		},
		owners: map[int]int{100: 100, 101: 100, 200: 100, 300: 300},
		details: map[int]Process{
			100: {PID: 100, Name: "agent-overflow", RSSBytes: 10},
			101: {PID: 101, Name: "provider", RSSBytes: 20},
			200: {PID: 200, Name: "com.apple.WebKit.WebContent", RSSBytes: 30},
			300: {PID: 300, Name: "com.apple.WebKit.WebContent", RSSBytes: 40},
		},
	}
	tree, err := sampleDarwin(reader, 100, func(name string) bool {
		return matchesAnyPrefix(name, DefaultWebviewPrefixes)
	}, DefaultWebviewPrefixes)
	if err != nil {
		t.Fatal(err)
	}
	if got := tree.Children; !reflect.DeepEqual(got, []Process{{PID: 200, Name: "com.apple.WebKit.WebContent", RSSBytes: 30}}) {
		t.Fatalf("children = %#v", got)
	}
	if tree.TotalRSSBytes() != 40 {
		t.Fatalf("total = %d, want 40", tree.TotalRSSBytes())
	}
}

func TestSampleAllDarwinIncludesDescendantsAndResponsibleMembers(t *testing.T) {
	reader := fakeDarwinReader{
		rows: []darwinProcess{
			{pid: 100, ppid: 1, shortName: "agent-overflow"},
			{pid: 101, ppid: 100, shortName: "provider"},
			{pid: 102, ppid: 101, shortName: "git"},
			{pid: 150, ppid: 100, shortName: "Google Chrome"},
			{pid: 151, ppid: 1, shortName: "Google Chrome Hel"},
			{pid: 200, ppid: 1, shortName: "com.apple.WebKit"},
			{pid: 300, ppid: 1, shortName: "foreign"},
		},
		owners: map[int]int{100: 100, 101: 100, 102: 100, 150: 150, 151: 150, 200: 100, 300: 300},
		details: map[int]Process{
			100: {PID: 100, Name: "agent-overflow", RSSBytes: 10},
			101: {PID: 101, Name: "provider", RSSBytes: 20},
			102: {PID: 102, Name: "git", RSSBytes: 5},
			150: {PID: 150, Name: "Google Chrome", RSSBytes: 7},
			151: {PID: 151, Name: "Google Chrome Helper", RSSBytes: 8},
			200: {PID: 200, Name: "com.apple.WebKit.WebContent", RSSBytes: 30},
			300: {PID: 300, Name: "foreign", RSSBytes: 100},
		},
	}
	tree, err := sampleDarwin(reader, 100, func(string) bool { return true }, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]int, 0, len(tree.Children))
	for _, proc := range tree.Children {
		got = append(got, proc.PID)
	}
	if !reflect.DeepEqual(got, []int{101, 102, 150, 151, 200}) {
		t.Fatalf("pids = %v", got)
	}
	if tree.TotalRSSBytes() != 80 {
		t.Fatalf("total = %d, want 80", tree.TotalRSSBytes())
	}
}

func TestSampleDarwinFallsBackToDescendantsWithoutResponsibility(t *testing.T) {
	reader := fakeDarwinReader{
		rows: []darwinProcess{
			{pid: 100, ppid: 1, shortName: "agent-overflow"},
			{pid: 101, ppid: 100, shortName: "com.apple.WebKit"},
			{pid: 200, ppid: 1, shortName: "com.apple.WebKit"},
		},
		ownerErr: errors.New("unsupported"),
		details: map[int]Process{
			100: {PID: 100, Name: "agent-overflow", RSSBytes: 10},
			101: {PID: 101, Name: "com.apple.WebKit.WebContent", RSSBytes: 20},
			200: {PID: 200, Name: "com.apple.WebKit.WebContent", RSSBytes: 30},
		},
	}
	tree, err := sampleDarwin(reader, 100, func(name string) bool {
		return matchesAnyPrefix(name, DefaultWebviewPrefixes)
	}, DefaultWebviewPrefixes)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Children) != 1 || tree.Children[0].PID != 101 {
		t.Fatalf("children = %#v", tree.Children)
	}
}

func TestDarwinNativeSamplerReadsCurrentProcess(t *testing.T) {
	tree, err := SampleAll(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if tree.Self.PID != os.Getpid() || tree.Self.RSSBytes == 0 {
		t.Fatalf("self = %#v", tree.Self)
	}
}
