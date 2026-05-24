package sysstat

import (
	"context"
	"errors"
	"testing"

	"github.com/shirou/gopsutil/v4/mem"
)

func TestFirstOrZero(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []float64
		want float64
	}{
		{"nil", nil, 0},
		{"empty", []float64{}, 0},
		{"single", []float64{42.5}, 42.5},
		{"multiple takes head", []float64{17.25, 99.9}, 17.25},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := firstOrZero(tc.in); got != tc.want {
				t.Fatalf("firstOrZero(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSample_PropagatesShape(t *testing.T) {
	withFakes(t,
		func(context.Context) ([]float64, error) { return []float64{37.5}, nil },
		func(context.Context) (*mem.VirtualMemoryStat, error) {
			return &mem.VirtualMemoryStat{Used: 4 << 30, Total: 16 << 30}, nil
		},
	)

	got, err := Sample(context.Background())
	if err != nil {
		t.Fatalf("Sample returned error: %v", err)
	}
	want := Reading{CPUPercent: 37.5, MemUsedBytes: 4 << 30, MemTotalBytes: 16 << 30}
	if got != want {
		t.Fatalf("Sample = %+v, want %+v", got, want)
	}
}

func TestSample_CPUErrorPropagates(t *testing.T) {
	cpuErr := errors.New("cpu boom")
	withFakes(t,
		func(context.Context) ([]float64, error) { return nil, cpuErr },
		func(context.Context) (*mem.VirtualMemoryStat, error) {
			t.Fatal("mem read should not run after CPU error")
			return nil, nil
		},
	)

	if _, err := Sample(context.Background()); !errors.Is(err, cpuErr) {
		t.Fatalf("Sample err = %v, want %v", err, cpuErr)
	}
}

func TestSample_MemErrorPropagates(t *testing.T) {
	memErr := errors.New("mem boom")
	withFakes(t,
		func(context.Context) ([]float64, error) { return []float64{12}, nil },
		func(context.Context) (*mem.VirtualMemoryStat, error) { return nil, memErr },
	)

	if _, err := Sample(context.Background()); !errors.Is(err, memErr) {
		t.Fatalf("Sample err = %v, want %v", err, memErr)
	}
}

func TestSample_EmptyCPUSliceFoldsToZero(t *testing.T) {
	withFakes(t,
		func(context.Context) ([]float64, error) { return []float64{}, nil },
		func(context.Context) (*mem.VirtualMemoryStat, error) {
			return &mem.VirtualMemoryStat{Used: 1, Total: 2}, nil
		},
	)

	got, err := Sample(context.Background())
	if err != nil {
		t.Fatalf("Sample returned error: %v", err)
	}
	if got.CPUPercent != 0 {
		t.Fatalf("CPUPercent = %v, want 0 (empty slice → 0)", got.CPUPercent)
	}
}

func withFakes(
	t *testing.T,
	cpuFn func(context.Context) ([]float64, error),
	memFn func(context.Context) (*mem.VirtualMemoryStat, error),
) {
	t.Helper()
	origCPU := readCPUPercent
	origMem := readMem
	readCPUPercent = cpuFn
	readMem = memFn
	t.Cleanup(func() {
		readCPUPercent = origCPU
		readMem = origMem
	})
}
