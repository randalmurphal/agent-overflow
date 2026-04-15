package provider

import (
	"context"
	"io"
	"testing"
	"time"
)

func TestSpawnAndEcho(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn cat: %v", err)
	}
	defer p.Kill()

	msg := []byte(`{"hello":"world"}`)
	if err := p.WriteLine(msg); err != nil {
		t.Fatalf("write: %v", err)
	}

	line, err := p.ReadLine()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if string(line) != string(msg) {
		t.Errorf("got %q, want %q", line, msg)
	}
}

func TestSpawnMultipleLines(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn cat: %v", err)
	}
	defer p.Kill()

	lines := []string{"first", "second", "third"}
	for _, l := range lines {
		if err := p.WriteLine([]byte(l)); err != nil {
			t.Fatalf("write %q: %v", l, err)
		}
	}

	for _, want := range lines {
		got, err := p.ReadLine()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestCloseGraceful(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn cat: %v", err)
	}

	// cat exits when stdin closes, so Close should succeed via step 1 (stdin close).
	if err := p.Close(); err != nil {
		// cat returns exit status 0 on stdin close, so err should be nil
		t.Logf("close returned: %v (expected for cat)", err)
	}

	// Done channel should be closed after Close.
	select {
	case <-p.Done():
		// ok
	case <-time.After(5 * time.Second):
		t.Fatal("Done channel not closed after Close()")
	}
}

func TestKillImmediate(t *testing.T) {
	ctx := context.Background()
	// sleep won't exit on stdin close, so Kill is the only way.
	p, err := Spawn(ctx, SpawnConfig{Binary: "sleep", Args: []string{"60"}})
	if err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}

	start := time.Now()
	p.Kill()
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("Kill took %v, expected < 2s", elapsed)
	}

	select {
	case <-p.Done():
		// ok
	default:
		t.Fatal("Done channel not closed after Kill()")
	}
}

func TestReadLineEOFAfterExit(t *testing.T) {
	ctx := context.Background()
	// echo outputs one line then exits.
	p, err := Spawn(ctx, SpawnConfig{Binary: "echo", Args: []string{"hello"}})
	if err != nil {
		t.Fatalf("spawn echo: %v", err)
	}

	line, err := p.ReadLine()
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if string(line) != "hello" {
		t.Errorf("got %q, want %q", line, "hello")
	}

	// Next read should return EOF.
	_, err = p.ReadLine()
	if err != io.EOF {
		t.Errorf("expected io.EOF, got %v", err)
	}

	// Wait for process to finish.
	<-p.Done()
}

func TestWriteLineAfterExit(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{Binary: "true"})
	if err != nil {
		t.Fatalf("spawn true: %v", err)
	}

	// Wait for process to exit.
	<-p.Done()

	err = p.WriteLine([]byte("should fail"))
	if err == nil {
		t.Fatal("expected error writing to exited process, got nil")
	}
}

func TestSetpgid(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn cat: %v", err)
	}
	defer p.Kill()

	// Verify Setpgid was configured by checking the SysProcAttr.
	if p.cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil")
	}
	if !p.cmd.SysProcAttr.Setpgid {
		t.Error("Setpgid not set")
	}
}

func TestSpawnWithEnv(t *testing.T) {
	ctx := context.Background()
	// Use env to print a specific variable.
	p, err := Spawn(ctx, SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", "echo $TEST_VAR"},
		Env:    map[string]string{"TEST_VAR": "hello_from_test"},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	line, err := p.ReadLine()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if string(line) != "hello_from_test" {
		t.Errorf("got %q, want %q", line, "hello_from_test")
	}

	<-p.Done()
}

func TestSpawnWithDir(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{
		Binary: "pwd",
		Dir:    "/tmp",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	line, err := p.ReadLine()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// /tmp might resolve to /private/tmp on macOS.
	got := string(line)
	if got != "/tmp" && got != "/private/tmp" {
		t.Errorf("got %q, want /tmp or /private/tmp", got)
	}

	<-p.Done()
}

func TestSpawnInvalidBinary(t *testing.T) {
	ctx := context.Background()
	_, err := Spawn(ctx, SpawnConfig{Binary: "/nonexistent/binary"})
	if err == nil {
		t.Fatal("expected error for nonexistent binary, got nil")
	}
}

func TestReadLineReturnsCopy(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer p.Kill()

	if err := p.WriteLine([]byte("first")); err != nil {
		t.Fatalf("write: %v", err)
	}

	line1, err := p.ReadLine()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// Save the content before next read.
	saved := string(line1)

	if err := p.WriteLine([]byte("second")); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err = p.ReadLine()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// The first line's data should still be intact (not overwritten by scanner reuse).
	if string(line1) != saved {
		t.Errorf("ReadLine did not return a copy: first line mutated from %q to %q", saved, string(line1))
	}
}

func TestDoneChannelCloses(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{Binary: "true"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	select {
	case <-p.Done():
		// ok — process exited
	case <-time.After(5 * time.Second):
		t.Fatal("Done channel not closed within 5s")
	}
}

func TestErrAccessor(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{Binary: "false"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	<-p.Done()

	// `false` exits with code 1, so Err should be non-nil.
	if p.Err() == nil {
		t.Error("expected non-nil error from `false`, got nil")
	}
}
