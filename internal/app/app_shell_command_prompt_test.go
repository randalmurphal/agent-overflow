//go:build unix

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/shellrun"

	"github.com/google/uuid"
)

// lockedLog is a log destination a test reads while goroutines still write.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// A password typed at a command's prompt reaches the command and nothing the
// app keeps or sends: not the message to the agent, the stored row, the
// attachment, a frame, an error or the log.
func TestAnAnswerToACommandsPromptReachesOnlyTheCommand(t *testing.T) {
	app, rec := newShellCommandApp(t)
	logged := &lockedLog{}
	previous := log.Writer()
	log.SetOutput(logged)
	t.Cleanup(func() { log.SetOutput(previous) })
	thread, capturePath := newMixedTurnThread(t, app, "thread-shell-prompt")
	runID := uuid.NewString()
	// The way sudo and ssh ask: echo off, the prompt on the terminal, one
	// line read from it.
	command := `stty -echo < /dev/tty; printf 'Password: ' > /dev/tty; IFS= read -r pw < /dev/tty; stty echo < /dev/tty; echo "length=${#pw}"`
	if _, err := app.RunThreadShellCommand(context.Background(), thread.ID, ShellCommandRequest{RunID: runID, Command: command}); err != nil {
		t.Fatal(err)
	}

	// The prompt is in the snapshot a client reads and on the stream.
	deadline := time.Now().Add(15 * time.Second)
	for {
		runs, err := app.GetThreadShellCommands(thread.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) == 1 && runs[0].Terminal == "Password: " && runs[0].TerminalSeq > 0 && runs[0].Output == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the prompt never reached the snapshot: %+v", runs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	prompted := false
	for _, frame := range shellFrames(rec, runID) {
		prompted = prompted || frame.Phase == shellrun.PhaseTerminal && frame.Terminal == "Password: " && frame.TerminalSeq > 0
	}
	if !prompted {
		t.Fatalf("no terminal frame carried the prompt: %+v", shellFrames(rec, runID))
	}

	const secret = "hunter2-correct-horse"
	refused := app.AnswerThreadShellCommand(thread.ID, ShellCommandAnswer{RunID: runID, Text: secret + "\n" + secret})
	if !errors.Is(refused, shellrun.ErrAnswerRejected) || strings.Contains(refused.Error(), secret) {
		t.Fatalf("an answer of two lines: %v", refused)
	}
	if err := app.AnswerThreadShellCommand(thread.ID, ShellCommandAnswer{RunID: uuid.NewString(), Text: secret}); !errors.Is(err, shellrun.ErrNotRunning) {
		t.Fatalf("an answer for another run: %v", err)
	}
	if err := app.AnswerThreadShellCommand(thread.ID, ShellCommandAnswer{RunID: runID, Text: secret}); err != nil {
		t.Fatal(err)
	}
	if final := waitShellFinished(t, rec, runID); final.State != shellrun.StateSent || final.ExitCode != 0 {
		t.Fatalf("terminal frame: %+v", final)
	}
	if err := app.AnswerThreadShellCommand(thread.ID, ShellCommandAnswer{RunID: runID, Text: secret}); !errors.Is(err, shellrun.ErrNotRunning) {
		t.Fatalf("an answer after the command ended: %v", err)
	}

	// The command got the whole answer, and the agent gets the command's
	// output: neither the answer nor the prompt.
	texts := waitForCapturedUserMessages(t, capturePath, 1)
	want := "<bash-input>" + command + "</bash-input>\n<bash-stdout>length=21</bash-stdout><bash-stderr></bash-stderr>"
	if texts[0] != want {
		t.Fatalf("wire text =\n%q\nwant\n%q", texts[0], want)
	}
	wire, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatal(err)
	}
	rows := userItems(t, app, thread.ID)
	stored, err := json.Marshal(rows)
	if err != nil || len(rows) != 1 {
		t.Fatalf("user rows: %+v %v", rows, err)
	}
	meta, err := usermessageMetaFromItem(rows[0])
	if err != nil || len(meta.Attachments) != 1 {
		t.Fatalf("row meta: %+v %v", meta, err)
	}
	page, err := app.ReadShellAttachmentOutput(thread.ID, meta.Attachments[0].ID, 0, 0)
	if err != nil || page.Data != "length=21\n" {
		t.Fatalf("output page: %+v %v", page, err)
	}
	frames, err := json.Marshal(rec.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	for place, text := range map[string]string{
		"the provider's input": string(wire),
		"the stored row":       string(stored),
		"the output":           page.Data + page.Command,
		"an emitted frame":     string(frames),
		"the log":              logged.String(),
	} {
		if strings.Contains(text, secret) {
			t.Errorf("the answer reached %s", place)
		}
	}
	// The wire text above is exact, so the prompt is not in what the agent
	// reads as output either.
	if strings.Contains(page.Data, "Password: ") {
		t.Error("the prompt reached the output")
	}
}
