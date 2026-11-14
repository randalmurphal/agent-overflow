//go:build !windows

package shellrun

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/procutil"
)

// terminalFrame waits for a terminal frame whose text holds marker.
func (r *recorder) terminalFrame(t *testing.T, marker string) Event {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		events, _, _ := r.snapshot()
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].Phase == PhaseTerminal && strings.Contains(events[i].Terminal, marker) {
				return events[i]
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	events, _, _ := r.snapshot()
	t.Fatalf("no terminal frame holds %q; frames: %+v", marker, events)
	return Event{}
}

// everywhere is every string a run put on the wire or handed to Finish.
func (r *recorder) everywhere() string {
	events, results, outputs := r.snapshot()
	var all strings.Builder
	for _, event := range events {
		all.WriteString(event.Chunk + "\x00" + event.Terminal + "\x00" + event.Error + "\x00" + event.Command + "\x00")
	}
	for _, result := range results {
		all.WriteString(result.Command + "\x00")
	}
	for _, output := range outputs {
		all.WriteString(output + "\x00")
	}
	return all.String()
}

// A password prompt, the way sudo and ssh ask: echo off, the prompt written
// to the terminal, one line read from it.
const passwordPrompt = `stty -echo < /dev/tty; printf 'Password: ' > /dev/tty; IFS= read -r pw < /dev/tty; stty echo < /dev/tty; echo "length=${#pw}"`

func TestAPromptIsShownAndItsAnswerReachesOnlyTheCommand(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	service := newService(t, rec, nil)
	request := shellRequest(t, "run-1", passwordPrompt)
	if _, err := service.Start(request); err != nil {
		t.Fatal(err)
	}
	frame := rec.terminalFrame(t, "Password: ")
	if frame.Terminal != "Password: " || frame.TerminalSeq == 0 || frame.Chunk != "" || frame.Seq != 0 {
		t.Fatalf("terminal frame: %+v", frame)
	}
	// A client that arrives now reads the prompt from the snapshot.
	active := service.Active("thread-1")
	if len(active) != 1 || active[0].Terminal != "Password: " || active[0].TerminalSeq != frame.TerminalSeq || active[0].Output != "" {
		t.Fatalf("snapshot: %+v", active)
	}

	const secret = "hunter2-correct-horse"
	if err := service.Answer("thread-1", "run-1", secret); err != nil {
		t.Fatal(err)
	}
	if terminal := rec.terminal(t); terminal.State != StateSent || terminal.ExitCode != 0 {
		t.Fatalf("terminal frame: %+v", terminal)
	}
	_, results, outputs := rec.snapshot()
	if len(results) != 1 || outputs[0] != "length=21\n" {
		t.Fatalf("results=%+v outputs=%q", results, outputs)
	}
	if everywhere := rec.everywhere(); strings.Contains(everywhere, secret) {
		t.Fatalf("the answer left the terminal: %q", everywhere)
	}
	events, _, _ := rec.snapshot()
	finished := false
	for _, event := range events {
		if event.Phase == PhaseOutput && strings.Contains(event.Chunk, "Password") {
			t.Fatalf("the prompt reached the output: %+v", event)
		}
		if finished && event.Phase == PhaseTerminal {
			t.Fatalf("a terminal frame followed the finished frame: %+v", events)
		}
		finished = finished || event.Phase == PhaseFinished
	}
}

// A program that leaves echo on shows what was typed, as a terminal does.
// That text is still not output.
func TestAnEchoedAnswerShowsOnTheTerminalAndNotInTheOutput(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	service := newService(t, rec, nil)
	request := shellRequest(t, "run-1", `printf 'Continue? ' > /dev/tty; IFS= read -r a < /dev/tty; echo "a=$a"; printf 'done\n' > /dev/tty; sleep 0.3`)
	if _, err := service.Start(request); err != nil {
		t.Fatal(err)
	}
	rec.terminalFrame(t, "Continue? ")
	if err := service.Answer("thread-1", "run-1", "yes"); err != nil {
		t.Fatal(err)
	}
	// One line end per line: the terminal's CR LF is reduced to LF.
	frame := rec.terminalFrame(t, "done\n")
	if frame.Terminal != "Continue? yes\ndone\n" {
		t.Fatalf("terminal text = %q", frame.Terminal)
	}
	rec.terminal(t)
	if _, _, outputs := rec.snapshot(); len(outputs) != 1 || outputs[0] != "a=yes\n" {
		t.Fatalf("outputs = %q", outputs)
	}
}

func TestAnswerRefusesWhatIsNotOneLineForTheRunningCommand(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	service := newService(t, rec, nil)
	if err := service.Answer("thread-1", "run-1", "x"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("answer with no run: %v", err)
	}
	if _, err := service.Start(shellRequest(t, "run-1", passwordPrompt)); err != nil {
		t.Fatal(err)
	}
	rec.terminalFrame(t, "Password: ")
	for name, text := range map[string]string{
		"a newline":          "two\nlines",
		"a carriage return":  "two\rlines",
		"an interrupt":       "abc\x03",
		"a delete":           "abc\x7f",
		"more than the line": strings.Repeat("x", AnswerLimitBytes+1),
	} {
		err := service.Answer("thread-1", "run-1", text)
		if !errors.Is(err, ErrAnswerRejected) {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(err.Error(), text) {
			t.Fatalf("%s: the error names the answer: %v", name, err)
		}
	}
	if err := service.Answer("thread-1", "another-run", "x"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("answer for another run: %v", err)
	}
	if err := service.Answer("another-thread", "run-1", "x"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("answer for another thread: %v", err)
	}
	// The refused answers typed nothing: the command still reads its one line.
	if err := service.Answer("thread-1", "run-1", strings.Repeat("y", AnswerLimitBytes)); err != nil {
		t.Fatalf("an answer of exactly the limit: %v", err)
	}
	rec.terminal(t)
	if _, _, outputs := rec.snapshot(); len(outputs) != 1 || outputs[0] != "length=512\n" {
		t.Fatalf("outputs = %q", outputs)
	}
	if err := service.Answer("thread-1", "run-1", "x"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("answer after the command ended: %v", err)
	}
}

// The terminal lasts exactly as long as the command: by the time the result
// is handed over it is closed, and an answer has nowhere to go.
func TestTheTerminalIsClosedOnceTheCommandIsGone(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	var device *procutil.Terminal
	var service *Service
	answered := make(chan error, 1)
	rec.finish = func(result Result) error {
		answered <- service.Answer(result.ThreadID, result.RunID, "late")
		return nil
	}
	service = newService(t, rec, func(config *Config) {
		config.OpenTerminal = func() (*procutil.Terminal, error) {
			opened, err := procutil.OpenTerminal()
			device = opened
			return opened, err
		}
	})
	if _, err := service.Start(shellRequest(t, "run-1", "echo hi")); err != nil {
		t.Fatal(err)
	}
	if terminal := rec.terminal(t); terminal.State != StateSent {
		t.Fatalf("terminal frame: %+v", terminal)
	}
	if err := <-answered; !errors.Is(err, ErrNotRunning) {
		t.Fatalf("an answer while the result is handed over: %v", err)
	}
	if _, err := device.Write([]byte("x")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("the terminal was left open: write err = %v", err)
	}
}

func TestACommandIsNotRunWhenItsTerminalCannotBeOpened(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	ran := false
	opening, release := make(chan struct{}), make(chan struct{})
	service := newService(t, rec, func(config *Config) {
		config.OpenTerminal = func() (*procutil.Terminal, error) {
			close(opening)
			<-release
			return nil, errors.New("out of ptys")
		}
		run := procutil.RunGroup
		config.Run = func(ctx context.Context, command procutil.GroupCommand) (procutil.GroupOutcome, error) {
			ran = true
			return run(ctx, command)
		}
	})
	if _, err := service.Start(shellRequest(t, "run-1", "echo hi")); err != nil {
		t.Fatal(err)
	}
	// The run is registered before its terminal exists.
	<-opening
	if err := service.Answer("thread-1", "run-1", "x"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("answer before the terminal exists: %v", err)
	}
	close(release)
	terminal := rec.terminal(t)
	if terminal.State != StateFailed || terminal.Error != "Could not give the command a terminal: out of ptys" {
		t.Fatalf("terminal frame: %+v", terminal)
	}
	if ran || service.HasActive() {
		t.Fatalf("ran=%v active=%v", ran, service.HasActive())
	}
	if err := service.Answer("thread-1", "run-1", "x"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("answer for a run with no terminal: %v", err)
	}
}

func newTestTerminalView(rec *recorder) *terminalView {
	return newTerminalView(&run{request: Request{ThreadID: "thread-1", RunID: "run-1"}}, rec.emit)
}

func terminalFrames(rec *recorder) []Event {
	events, _, _ := rec.snapshot()
	return events
}

func TestTheTerminalViewSendsTheFirstWriteAtOnceAndPacesTheRest(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	view := newTestTerminalView(rec)
	_, _ = view.Write([]byte("Password: "))
	frames := terminalFrames(rec)
	if len(frames) != 1 || frames[0].Phase != PhaseTerminal || frames[0].Terminal != "Password: " ||
		frames[0].TerminalSeq != 1 || frames[0].ThreadID != "thread-1" || frames[0].RunID != "run-1" {
		t.Fatalf("first write: %+v", frames)
	}
	for _, part := range []string{"\r\n", "Sorry, try again.\r\n", "Password: "} {
		_, _ = view.Write([]byte(part))
	}
	if frames := terminalFrames(rec); len(frames) != 1 {
		t.Fatalf("writes inside the interval were not held: %+v", frames)
	}
	// The snapshot is ahead of the last frame, under that frame's sequence.
	if text, seq := view.snapshot(); text != "Password: \nSorry, try again.\nPassword: " || seq != 1 {
		t.Fatalf("snapshot = %q, %d", text, seq)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(terminalFrames(rec)) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the held writes were never sent")
		}
		time.Sleep(5 * time.Millisecond)
	}
	frames = terminalFrames(rec)
	if frames[1].Terminal != "Password: \nSorry, try again.\nPassword: " || frames[1].TerminalSeq != 2 {
		t.Fatalf("second frame: %+v", frames[1])
	}
	// Nothing new: the timer that follows a frame sends nothing.
	time.Sleep(3 * flushInterval)
	if frames := terminalFrames(rec); len(frames) != 2 {
		t.Fatalf("a frame with nothing new: %+v", frames)
	}
	// Quiet again, so the next write is sent at once.
	_, _ = view.Write([]byte("x"))
	if frames := terminalFrames(rec); len(frames) != 3 || frames[2].TerminalSeq != 3 {
		t.Fatalf("a write after the interval: %+v", frames)
	}
}

func TestTheTerminalViewSendsNothingOnceClosed(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	view := newTestTerminalView(rec)
	_, _ = view.Write([]byte("a"))
	_, _ = view.Write([]byte("held"))
	view.close()
	if n, err := view.Write([]byte("late")); n != 4 || err != nil {
		t.Fatalf("a write to a closed view: %d %v", n, err)
	}
	time.Sleep(3 * flushInterval)
	if frames := terminalFrames(rec); len(frames) != 1 || frames[0].Terminal != "a" {
		t.Fatalf("frames after close: %+v", frames)
	}

	// A view that was quiet when it closed would send its next write at once.
	quietRec := &recorder{}
	quiet := newTestTerminalView(quietRec)
	_, _ = quiet.Write([]byte("a"))
	time.Sleep(3 * flushInterval)
	quiet.close()
	_, _ = quiet.Write([]byte("late"))
	if frames := terminalFrames(quietRec); len(frames) != 1 {
		t.Fatalf("a quiet view sent after close: %+v", frames)
	}
}

func TestTheTerminalViewKeepsABoundedEndOfWholeRunes(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	view := newTestTerminalView(rec)
	// A rune the terminal has only begun is left for the frame that ends it.
	_, _ = view.Write([]byte("caf\xc3"))
	if frames := terminalFrames(rec); frames[0].Terminal != "caf" {
		t.Fatalf("a begun rune: %q", frames[0].Terminal)
	}
	_, _ = view.Write([]byte("\xa9"))
	if text, _ := view.snapshot(); text != "café" {
		t.Fatalf("the finished rune: %q", text)
	}
	// Past the bound only the end is kept. The bound is not a multiple of
	// this rune's three bytes, so the cut lands inside one, which is dropped.
	_, _ = view.Write([]byte(strings.Repeat("€", terminalTailBytes)))
	text, _ := view.snapshot()
	if len(text) > terminalTailBytes || len(text) < terminalTailBytes-utf8.UTFMax || !utf8.ValidString(text) ||
		strings.Trim(text, "€") != "" {
		t.Fatalf("bounded text: %d bytes, valid=%v", len(text), utf8.ValidString(text))
	}
}
