package supervise

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
)

// ProtocolVersion is what the two processes agree on. It is part of the safety
// boundary rather than a courtesy: a target that needs a database snapshot is
// refused when the INSTALLED supervisor is older than the protocol that
// promises one, and the remedy is a local `agent-overflow service update`,
// which replaces the supervisor with the operator standing there.
const ProtocolVersion = 1

// RestartForUpdateExitCode asks the supervisor to re-read its durable update
// selection after the backend has drained safely. Older supervisors propagate
// the failure to the service manager, whose on-failure restart does the same.
const RestartForUpdateExitCode = 75

// ErrUpdateOutcomeUnknown means the request may already be durably accepted.
// The child must stop accepting work and restart through its supervisor; a
// timeout or broken pipe is not an explicit refusal and cannot permit retry.
var ErrUpdateOutcomeUnknown = errors.New("the supervisor's update result is unconfirmed")

// The message types, in one closed set. Kept small on purpose: every frame
// here is a state transition the supervisor's single loop must serialize, and
// a vocabulary that grows is a state machine that grows with it.
const (
	// MsgActivate is the supervisor's opening frame, written at spawn. It is
	// what tells a child whether it is a trial.
	MsgActivate = "activate"
	// MsgHello is the child's answer: which protocol and version actually
	// booted. The supervisor checks the protocol and logs the version.
	MsgHello = "hello"
	// MsgRequestUpdate asks to run an already-staged version.
	MsgRequestUpdate = "request-update"
	// MsgUpdateAccepted answers it: the update is durably pending under this
	// id, and this child is about to be stopped.
	MsgUpdateAccepted = "update-accepted"
	// MsgUpdateRefused answers it the other way, with the reason. Without it a
	// caller waiting on an answer would wait forever for a target that never
	// existed.
	MsgUpdateRefused = "update-refused"
	// MsgPrepared is the trial reporting that it booted fully and every
	// subsystem that could act unattended is parked at the activation gate.
	MsgPrepared = "prepared"
	// MsgCommit releases that gate. The supervisor sends it only after the
	// commit is durable.
	MsgCommit = "commit"
)

// Message is one JSON line on the pipe. One struct for both directions: the
// set is small, the fields are disjoint by type, and two structs would mean
// two encoders to keep in step for no property gained.
type Message struct {
	Type string `json:"type"`
	// ProtocolVersion rides activate and hello, the two frames that establish
	// whether the pair can talk at all.
	ProtocolVersion int `json:"protocolVersion,omitempty"`
	// Version is the child's own build version, on hello.
	Version string `json:"version,omitempty"`
	// Trial rides activate.
	Trial bool `json:"trial,omitempty"`
	// OwnsDataRoot rides activate. Its absence on an older supervisor means
	// the child must acquire its own lock, preserving mixed-version boot.
	OwnsDataRoot bool `json:"ownsDataRoot,omitempty"`
	// UpdateID rides activate (on a trial), update-accepted, prepared and
	// commit — the id the client correlates its reconnect against.
	UpdateID string `json:"updateId,omitempty"`
	// TargetVersion rides request-update, where it is the version being
	// asked for, and activate, where it is the version the update this boot
	// follows was aiming AT. The second reading is what lets a rolled-back
	// boot name the version that failed rather than the one that came back:
	// they are different versions, and only the record knows both.
	TargetVersion string `json:"targetVersion,omitempty"`
	// Outcome rides activate on an ordinary boot that FOLLOWS an update:
	// committed, rolled-back or failed. It is how a backend that did not run
	// the trial still knows what to tell the client that asked — a rollback's
	// whole point is that the version answering is not the one requested, and
	// it is the only one that can say so.
	Outcome string `json:"outcome,omitempty"`
	// Reason rides update-refused, and activate beside a settled Outcome.
	Reason string `json:"reason,omitempty"`
}

// EnvChannel names the environment variable that tells a child it has a
// supervisor, and which two descriptors carry it: "<read>,<write>".
//
// An environment marker rather than a bare descriptor probe, because a
// descriptor number is a guess. Anything can hand a process an open fd 3, and
// a supervisor's channel is not a thing to discover by accident — the marker
// says both THAT there is one and WHICH descriptors it is, in one value that
// only the spawning supervisor writes. The child unsets it the moment it reads
// it, so nothing it later spawns inherits a claim to a channel it does not
// hold.
const EnvChannel = "AO_SERVICE_CHANNEL"

// ChildFDs are the descriptors a spawned child inherits. 3 and 4 because 0-2
// are the child's own stdio: a serve host prints its endpoints to stdout for a
// person to read and takes a pairing answer on stdin, so neither is available.
const (
	ChildReadFD  = 3
	ChildWriteFD = 4
)

// ChannelEnvValue renders the marker's value.
func ChannelEnvValue() string {
	return strconv.Itoa(ChildReadFD) + "," + strconv.Itoa(ChildWriteFD)
}

// Conn is one end of the channel: JSON lines in, JSON lines out.
//
// Writes are serialized by a mutex because both the supervisor's loop and its
// timers can answer, and a torn line is an unparseable frame rather than a
// retryable one. Reads are single-consumer by construction — each side has one
// reader — so they are not locked.
type Conn struct {
	reader *bufio.Reader
	writer io.WriteCloser
	closer io.Closer

	writeMu sync.Mutex
	once    sync.Once
}

// NewConn builds a channel end over one read side and one write side.
func NewConn(r io.Reader, w io.WriteCloser, closer io.Closer) *Conn {
	return &Conn{reader: bufio.NewReader(r), writer: w, closer: closer}
}

// Send writes one frame.
func (c *Conn) Send(msg Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("supervise: encode %s: %w", msg.Type, err)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.writer.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("supervise: send %s: %w", msg.Type, err)
	}
	return nil
}

// Receive reads one frame. io.EOF means the other end closed, which for the
// supervisor is the child exiting and for the child is the supervisor dying.
func (c *Conn) Receive() (Message, error) {
	line, err := c.reader.ReadBytes('\n')
	if err != nil {
		if len(line) == 0 {
			return Message{}, err
		}
		// A final line with no newline is a torn write from a process that
		// died mid-frame. It is not a message.
		return Message{}, fmt.Errorf("supervise: truncated frame %q: %w", string(line), err)
	}
	var msg Message
	if err := json.Unmarshal(line, &msg); err != nil {
		return Message{}, fmt.Errorf("supervise: decode frame: %w", err)
	}
	if strings.TrimSpace(msg.Type) == "" {
		return Message{}, errors.New("supervise: frame carries no type")
	}
	return msg, nil
}

// Close releases both ends. Idempotent: the supervisor closes a channel when
// its child exits and again when it shuts down.
func (c *Conn) Close() error {
	var err error
	c.once.Do(func() {
		if closeErr := c.writer.Close(); closeErr != nil {
			err = closeErr
		}
		if c.closer != nil {
			if closeErr := c.closer.Close(); closeErr != nil && err == nil {
				err = closeErr
			}
		}
	})
	return err
}

// OpenChildChannel returns this process's end of a supervisor channel, or
// (nil, nil) when there is no supervisor.
//
// A supervisor is OPTIONAL forever: `agent-overflow serve` started by hand has
// no marker, gets no channel, and behaves exactly as it did before this
// package existed. That is why an absent marker is not an error.
//
// The descriptors are validated as pipes before use. A marker with no channel
// behind it is a broken spawn, and inheriting somebody else's fd 3 as a
// control channel is the one mistake worth failing on rather than logging.
func OpenChildChannel(lookupEnv func(string) (string, bool), unsetEnv func(string) error) (*Conn, error) {
	value, ok := lookupEnv(EnvChannel)
	if !ok || strings.TrimSpace(value) == "" {
		return nil, nil
	}
	// Unset before anything can fail, so a broken marker cannot be inherited
	// by a provider subprocess either.
	if unsetEnv != nil {
		if err := unsetEnv(EnvChannel); err != nil {
			return nil, fmt.Errorf("supervise: clear %s: %w", EnvChannel, err)
		}
	}
	readText, writeText, found := strings.Cut(value, ",")
	if !found {
		return nil, fmt.Errorf("supervise: %s = %q is not \"<read>,<write>\"", EnvChannel, value)
	}
	readFD, err := strconv.Atoi(strings.TrimSpace(readText))
	if err != nil {
		return nil, fmt.Errorf("supervise: %s read descriptor %q: %w", EnvChannel, readText, err)
	}
	writeFD, err := strconv.Atoi(strings.TrimSpace(writeText))
	if err != nil {
		return nil, fmt.Errorf("supervise: %s write descriptor %q: %w", EnvChannel, writeText, err)
	}
	read := os.NewFile(uintptr(readFD), "supervisor-read")
	write := os.NewFile(uintptr(writeFD), "supervisor-write")
	if read == nil || write == nil {
		return nil, fmt.Errorf("supervise: %s names descriptors %d,%d, which are not open", EnvChannel, readFD, writeFD)
	}
	for name, file := range map[string]*os.File{"read": read, "write": write} {
		info, err := file.Stat()
		if err != nil {
			return nil, fmt.Errorf("supervise: %s descriptor: %w", name, err)
		}
		if info.Mode()&os.ModeNamedPipe == 0 {
			return nil, fmt.Errorf("supervise: %s descriptor %s is not a pipe", name, file.Name())
		}
	}
	return NewConn(read, write, read), nil
}

// PreflightSubcommand is the argv a staged binary answers with what it is.
//
// It exists so the supervisor can ask a version it has never run whether the
// two can talk BEFORE anything is written down — the check t3code puts before
// the staging directory is renamed into place. Double-underscored like the
// other internal re-execs (`__reap`), because nobody types it.
const PreflightSubcommand = "__service-preflight"

// Preflight is what that subcommand prints: one JSON object, one line, then
// exit 0.
type Preflight struct {
	ProtocolVersion int    `json:"protocolVersion"`
	Version         string `json:"version"`
}

// WritePreflight renders this binary's answer.
func WritePreflight(w io.Writer, version string) error {
	data, err := json.Marshal(Preflight{ProtocolVersion: ProtocolVersion, Version: version})
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// ParsePreflight reads one back, tolerating the leading log lines a binary may
// print before it gets to the answer: the LAST non-empty line is the answer,
// which is the same rule the structured-output decoders in this tree use.
func ParsePreflight(output string) (Preflight, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var answer Preflight
		if err := json.Unmarshal([]byte(line), &answer); err != nil {
			return Preflight{}, fmt.Errorf("supervise: preflight answer %q: %w", line, err)
		}
		if answer.ProtocolVersion <= 0 {
			return Preflight{}, fmt.Errorf("supervise: preflight answer names no protocol version")
		}
		return answer, nil
	}
	return Preflight{}, errors.New("supervise: preflight printed nothing")
}

// CheckPreflight is the compatibility rule, stated once.
//
// A target that speaks a NEWER protocol is refused, because whatever it needs
// the newer protocol for is exactly what this supervisor cannot give it. A
// target that speaks an older one is accepted: this supervisor knows every
// frame that one can send. The refusal names the remedy, because an operator
// reading it in a journal has one move to make.
func CheckPreflight(answer Preflight) error {
	if answer.ProtocolVersion > ProtocolVersion {
		return fmt.Errorf(
			"the staged version speaks update protocol %d and this supervisor speaks %d: "+
				"stop the service and run `agent-overflow service update` once to replace the supervisor, "+
				"then updates over the wire can resume",
			answer.ProtocolVersion, ProtocolVersion)
	}
	return nil
}
