package sessionimport

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestManagerBoundedImportsOverlapSmallSessionsAndRunLargeOnesAlone(t *testing.T) {
	initialStarted := make(chan string, managerWorkers)
	releaseInitial := make(chan struct{})
	largeStarted := make(chan struct{}, 1)
	releaseLarge := make(chan struct{})

	jobs := make([]managerJob, 0, managerWorkers+3)
	for i := range managerWorkers {
		id := "initial-" + strconv.Itoa(i)
		jobs = append(jobs, managerJob{id: id, found: true, row: Row{ID: id, ProjectID: "project", SizeBytes: 1}})
	}
	jobs = append(jobs,
		managerJob{id: "large", found: true, row: Row{ID: "large", ProjectID: "project", SizeBytes: managerSlotBytes * managerWorkers}},
		managerJob{id: "later-a", found: true, row: Row{ID: "later-a", ProjectID: "project", SizeBytes: 1}},
		managerJob{id: "later-b", found: true, row: Row{ID: "later-b", ProjectID: "project", SizeBytes: 1}},
	)

	var mu sync.Mutex
	running := 0
	largeRunning := false
	overlapped := ""
	manager := NewManager(ManagerConfig{})
	manager.importOne = func(_ context.Context, _ Deps, row Row) (ImportOutcome, error) {
		mu.Lock()
		running++
		isLarge := row.ID == "large"
		if overlapped == "" && (largeRunning || (isLarge && running > 1)) {
			overlapped = row.ID
		}
		if isLarge {
			largeRunning = true
		}
		mu.Unlock()
		switch {
		case strings.HasPrefix(row.ID, "initial-"):
			initialStarted <- row.ID
			<-releaseInitial
		case isLarge:
			largeStarted <- struct{}{}
			<-releaseLarge
		}
		mu.Lock()
		running--
		if isLarge {
			largeRunning = false
		}
		mu.Unlock()
		return ImportOutcome{}, nil
	}

	results := manager.runBounded(context.Background(), Deps{}, jobs)
	for range managerWorkers {
		select {
		case <-initialStarted:
		case <-time.After(time.Second):
			t.Fatalf("%d small imports did not overlap", managerWorkers)
		}
	}
	select {
	case <-largeStarted:
		t.Fatal("large import started while small imports held every slot")
	default:
	}
	close(releaseInitial)
	select {
	case <-largeStarted:
	case <-time.After(time.Second):
		t.Fatal("large import did not start after small imports released")
	}
	close(releaseLarge)
	completed := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("job %s: %v", result.job.id, result.err)
		}
		completed++
	}
	if completed != len(jobs) {
		t.Fatalf("completed = %d, want %d", completed, len(jobs))
	}
	mu.Lock()
	defer mu.Unlock()
	if overlapped != "" {
		t.Fatalf("import %s overlapped the exclusive large import", overlapped)
	}
}

func TestManagerWeightCapsAggregateSourceBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int64
		want int64
	}{
		{name: "unknown", size: 0, want: 1},
		{name: "one byte", size: 1, want: 1},
		{name: "one slot", size: managerSlotBytes, want: 1},
		{name: "over one slot", size: managerSlotBytes + 1, want: 2},
		{name: "three slots", size: 3 * managerSlotBytes, want: 3},
		{name: "full budget", size: managerWorkers * managerSlotBytes, want: managerWorkers},
		{name: "oversize", size: 10 * managerWorkers * managerSlotBytes, want: managerWorkers},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := managerWeight(tc.size); got != tc.want {
				t.Fatalf("managerWeight(%d) = %d, want %d", tc.size, got, tc.want)
			}
		})
	}
}
