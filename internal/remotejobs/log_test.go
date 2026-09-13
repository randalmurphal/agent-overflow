package remotejobs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"github.com/google/uuid"
)

func logOptions(t *testing.T, size int64) Options {
	t.Helper()
	return Options{LogDir: t.TempDir(), MaxJobBytes: size, MaxRetainedBytes: size * 5, MinFreeBytes: 1, freeBytes: func(string) (uint64, error) { return 1 << 40, nil }}
}
func logManager(t *testing.T, o Options, run Run) (*Manager, *store.Store) {
	t.Helper()
	st := storetest.Clone(t)
	m, err := New(context.Background(), st, run, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m, st
}
func startLogJob(t *testing.T, m *Manager) string {
	t.Helper()
	r := request()
	if _, err := m.Start("owner", uuid.NewString(), t.TempDir(), r); err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func TestDiskLogRingOffsetsRetentionAndRestart(t *testing.T) {
	o := logOptions(t, 16)
	m, st := logManager(t, o, func(_ context.Context, _ string, _ []string, out io.Writer) (Outcome, error) {
		for _, part := range []string{"0123456789", "ABCDEFGHIJK", "LMNOPQRSTUVWXYZ"} {
			_, _ = io.WriteString(out, part)
		}
		return Outcome{ExitCode: 0}, nil
	})
	id := startLogJob(t, m)
	receipt := settled(t, m, id)
	if !receipt.Truncated {
		t.Fatal("discarded disk prefix not reported")
	}
	chunk, err := m.ReadLog("owner", id, 0, 128)
	if err != nil || chunk.Text != "KLMNOPQRSTUVWXYZ" || chunk.Offset != 20 || chunk.NextOffset != 36 || !chunk.Truncated {
		t.Fatalf("ring=%+v err=%v", chunk, err)
	}
	tail, err := m.ReadLog("owner", id, -1, 5)
	if err != nil || tail.Text != "VWXYZ" {
		t.Fatalf("tail=%+v err=%v", tail, err)
	}
	if _, err = m.ReadLog("intruder", id, 0, 128); err == nil {
		t.Fatal("unauthorized log read")
	}
	if _, err = m.SearchLog("intruder", id, "XYZ", 0, 128); err == nil {
		t.Fatal("unauthorized log search")
	}
	m.Close()
	restarted, err := New(context.Background(), st, func(context.Context, string, []string, io.Writer) (Outcome, error) {
		t.Error("restarted manager reran accepted job")
		return Outcome{ExitCode: 0}, nil
	}, o)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	chunk, err = restarted.ReadLog("owner", id, 0, 128)
	if err != nil || chunk.Text != "KLMNOPQRSTUVWXYZ" {
		t.Fatalf("after restart=%+v err=%v", chunk, err)
	}
	info, err := os.Stat(filepath.Join(o.LogDir, id+".log"))
	if err != nil || info.Size() != 16+logHeaderBytes {
		t.Fatalf("file size=%v err=%v", info, err)
	}
}

func TestLogRetentionExpiresOnlyCompletedLogsAndPreservesReceipt(t *testing.T) {
	o := logOptions(t, 16)
	o.MaxRetainedBytes = 32
	m, _ := logManager(t, o, func(_ context.Context, _ string, _ []string, out io.Writer) (Outcome, error) {
		_, _ = io.WriteString(out, "abcdefghijklmnop")
		return Outcome{ExitCode: 0}, nil
	})
	first := startLogJob(t, m)
	settled(t, m, first)
	second := startLogJob(t, m)
	settled(t, m, second)
	info, err := m.Log("owner", first)
	if err != nil || !info.Expired {
		t.Fatalf("expired=%+v err=%v", info, err)
	}
	if receipt, err := m.Get("owner", first); err != nil || receipt.State != "succeeded" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	// Header overhead counts against the budget. The guarantee is an upper bound,
	// not an exact number of retained jobs for artificial byte-sized capacities.
	info, err = m.Log("owner", second)
	if err != nil || info.Expired {
		t.Fatalf("newest=%+v err=%v", info, err)
	}
}

func TestDiskFailureDrainsAndRecoversWithExplicitMissingRange(t *testing.T) {
	o := logOptions(t, 32)
	var free atomic.Uint64
	free.Store(1 << 40)
	o.freeBytes = func(string) (uint64, error) { return free.Load(), nil }
	s, err := newLogStore(o)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	writer, err := s.create(id)
	if err != nil {
		t.Fatal(err)
	}
	writer.Write([]byte("before"))
	free.Store(0)
	writer.checked = time.Time{}
	if n, err := writer.Write([]byte("discarded")); err != nil || n != 9 {
		t.Fatalf("write blocked child n=%d err=%v", n, err)
	}
	free.Store(1 << 40)
	writer.checked = time.Time{}
	writer.Write([]byte("after"))
	s.finish(id)
	err = s.withLog(id, func(l *jobLog) error {
		info := l.info(id)
		if info.TotalBytes != 20 || info.RetainedBytes != 5 || !info.Truncated || info.Error == "" {
			t.Fatalf("recovered log=%+v", info)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Closed descriptor is a write failure too, not an error propagated to the child.
	id = uuid.NewString()
	writer, err = s.create(id)
	if err != nil {
		t.Fatal(err)
	}
	writer.file.Close()
	if n, err := writer.Write([]byte("still drain")); n != 11 || err != nil {
		t.Fatalf("disk write failure n=%d err=%v", n, err)
	}
	s.finish(id)
}

func TestLogSearchPageBoundaryAndConcurrentReads(t *testing.T) {
	o := logOptions(t, 1024)
	release := make(chan struct{})
	started := make(chan struct{})
	m, _ := logManager(t, o, func(ctx context.Context, _ string, _ []string, out io.Writer) (Outcome, error) {
		_, _ = io.WriteString(out, "12345needle890needleEND")
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return Outcome{ExitCode: -1}, ctx.Err()
		}
		return Outcome{ExitCode: 0}, nil
	})
	id := startLogJob(t, m)
	<-started
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				if _, err := m.ReadLog("owner", id, -1, 16); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	var offsets []int64
	for offset := int64(0); ; {
		result, err := m.SearchLog("owner", id, "needle", offset, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range result.Matches {
			offsets = append(offsets, match.Offset)
		}
		if result.Done {
			break
		}
		if result.NextOffset <= offset {
			t.Fatal("search did not advance")
		}
		offset = result.NextOffset
	}
	if len(offsets) != 2 || offsets[0] != 5 || offsets[1] != 14 {
		t.Fatalf("matches=%v", offsets)
	}
	close(release)
	settled(t, m, id)
}

func TestExplicitScriptsAndUnlimitedJobs(t *testing.T) {
	script := "printf '%s\\n' '$literal; no interpolation'\n" + strings.Repeat("# long script\n", 10000)
	var savedPath string
	m, _ := logManager(t, logOptions(t, 1024), func(ctx context.Context, cwd string, argv []string, out io.Writer) (Outcome, error) {
		if _, deadline := ctx.Deadline(); deadline {
			t.Error("unlimited context had deadline")
		}
		if len(argv) != 3 || argv[0] != "explicit-interpreter" || argv[1] != "--option" {
			t.Errorf("argv=%v", argv)
		}
		savedPath = argv[2]
		data, err := os.ReadFile(savedPath)
		if err != nil || string(data) != script {
			t.Errorf("script changed: %v", err)
		}
		info, err := os.Stat(savedPath)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Errorf("script permissions=%v %v", info, err)
		}
		return Outcome{ExitCode: 0}, nil
	})
	req := request()
	req.Argv = nil
	req.Script = script
	req.Interpreter = []string{"explicit-interpreter", "--option"}
	req.TimeoutSeconds = 0
	if _, err := m.Start("owner", uuid.NewString(), t.TempDir(), req); err != nil {
		t.Fatal(err)
	}
	settled(t, m, req.ID)
	if _, err := os.Stat(savedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("script retained: %v", err)
	}
	for _, change := range []func(*Request){func(r *Request) { r.Argv = []string{"also-argv"} }, func(r *Request) { r.Interpreter = nil }, func(r *Request) { r.TimeoutSeconds = -1 }, func(r *Request) { r.Script = strings.Repeat("x", (1<<20)+1) }, func(r *Request) { r.Interpreter = []string{"bad\x00arg"} }} {
		bad := req
		change(&bad)
		if Validate(bad) == nil {
			t.Fatalf("accepted malformed script request: %+v", bad)
		}
	}
}

func TestLogIncompleteOverwriteAndAdmissionFailureAreExplicit(t *testing.T) {
	o := logOptions(t, 16)
	m, _ := logManager(t, o, func(context.Context, string, []string, io.Writer) (Outcome, error) { return Outcome{ExitCode: 0}, nil })
	id := startLogJob(t, m)
	settled(t, m, id)
	file, err := os.OpenFile(filepath.Join(o.LogDir, id+".log"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteAt([]byte{1}, 33)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	// Both header verdicts are permanent, so neither may advise a retry.
	_, err = m.ReadLog("owner", id, 0, 16)
	if code, message, _ := errorsx.PublicDetails(err); code != "remote_log_unavailable" || !strings.Contains(message, "overwrite") || strings.Contains(message, "Retry") {
		t.Fatalf("interrupted overwrite: %v", err)
	}
	if file, err = os.OpenFile(filepath.Join(o.LogDir, id+".log"), os.O_RDWR, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteAt([]byte{0}, 33); err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteAt([]byte("BADMAGIC"), 0)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Log("owner", id)
	if code, message, _ := errorsx.PublicDetails(err); code != "remote_log_unavailable" || !strings.Contains(message, "damaged") || strings.Contains(message, "Retry") {
		t.Fatalf("damaged header: %v", err)
	}
	// Refuse before acceptance/spawn when no log can be reserved. The identical
	// ID is still usable once space is available; no phantom running receipt.
	var free atomic.Uint64
	o.freeBytes = func(string) (uint64, error) { return free.Load(), nil }
	m2, st := logManager(t, o, func(context.Context, string, []string, io.Writer) (Outcome, error) { return Outcome{ExitCode: 0}, nil })
	req := request()
	project := uuid.NewString()
	workspace := t.TempDir()
	if _, err = m2.Start("owner", project, workspace, req); err == nil {
		t.Fatal("admitted without log storage")
	}
	if _, err = st.GetRemoteJob(req.ID); !errors.Is(err, store.ErrRemoteJobNotFound) {
		t.Fatalf("unexpected receipt: %v", err)
	}
	free.Store(1 << 40)
	if _, err = m2.Start("owner", project, workspace, req); err != nil {
		t.Fatal(err)
	}
	settled(t, m2, req.ID)
}

func TestActiveLogReservationsCannotBeEvicted(t *testing.T) {
	o := logOptions(t, 64)
	o.MaxRetainedBytes = 64
	release := make(chan struct{})
	m, _ := logManager(t, o, func(ctx context.Context, _ string, _ []string, out io.Writer) (Outcome, error) {
		_, _ = io.WriteString(out, "active")
		select {
		case <-release:
			return Outcome{ExitCode: 0}, nil
		case <-ctx.Done():
			return Outcome{ExitCode: -1}, ctx.Err()
		}
	})
	first := startLogJob(t, m)
	req := request()
	if _, err := m.Start("owner", uuid.NewString(), t.TempDir(), req); err == nil {
		t.Fatal("admitted beyond reserved log capacity")
	}
	info, err := m.Log("owner", first)
	if err != nil || info.Expired {
		t.Fatalf("active log evicted %+v %v", info, err)
	}
	close(release)
	settled(t, m, first)
}

// A settled log is copied whole, in stream order, from a ring that wrapped;
// a running or expired job is refused with a stable code.
func TestSettledLogCopiesRetainedBytesAndRefusesRunningOrExpired(t *testing.T) {
	release := make(chan struct{})
	o := logOptions(t, 16)
	m, _ := logManager(t, o, func(ctx context.Context, _ string, _ []string, out io.Writer) (Outcome, error) {
		_, _ = io.WriteString(out, "0123456789ABCDEFGHIJ")
		select {
		case <-release:
		case <-ctx.Done():
		}
		_, _ = io.WriteString(out, "KLMNOPQRSTUVWXYZ")
		return Outcome{ExitCode: 0}, nil
	})
	id := startLogJob(t, m)
	if _, err := m.OpenSettledLog("owner", id); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("running job opened its log: %v", err)
	}
	close(release)
	settled(t, m, id)
	if _, err := m.OpenSettledLog("intruder", id); err == nil {
		t.Fatal("unauthorized settled log")
	}
	log, err := m.OpenSettledLog("owner", id)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if log.Info.RetainedBytes != 16 || log.Info.TotalBytes != 36 || !log.Info.Truncated || log.Receipt.ID != id {
		t.Fatalf("settled info: %+v receipt=%+v", log.Info, log.Receipt)
	}
	buf := make([]byte, 64)
	n, err := log.ReadAt(buf, 0)
	if err != nil || string(buf[:n]) != "KLMNOPQRSTUVWXYZ" {
		t.Fatalf("whole copy=%q err=%v", buf[:n], err)
	}
	n, err = log.ReadAt(buf[:5], 13)
	if err != nil || string(buf[:n]) != "XYZ" {
		t.Fatalf("partial copy=%q err=%v", buf[:n], err)
	}
	if _, err = log.ReadAt(buf, 16); err != io.EOF {
		t.Fatalf("read past retained end: %v", err)
	}
	if err = os.Remove(filepath.Join(o.LogDir, id+".log")); err != nil {
		t.Fatal(err)
	}
	if _, err = m.OpenSettledLog("owner", id); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("missing file not reported as expired: %v", err)
	}
}
