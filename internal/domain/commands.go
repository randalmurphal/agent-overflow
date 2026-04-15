package domain

// CommandKind identifies the type of orchestration command.
type CommandKind string

const (
	CmdCreateThread     CommandKind = "thread.create"
	CmdDeleteThread     CommandKind = "thread.delete"
	CmdSendMessage      CommandKind = "thread.message.send"
	CmdStartTurn        CommandKind = "thread.turn.start"
	CmdInterruptTurn    CommandKind = "thread.turn.interrupt"
	CmdCompleteTurn     CommandKind = "thread.turn.complete"
	CmdSetSession       CommandKind = "thread.session.set"
	CmdStopSession      CommandKind = "thread.session.stop"
	CmdAppendActivity   CommandKind = "thread.activity.append"
	CmdCompleteTurnDiff CommandKind = "thread.turn-diff.complete"
)

// Command is a request to change state, validated against the read model before producing events.
type Command struct {
	CommandID string      `json:"commandId"`
	Kind      CommandKind `json:"kind"`
	ThreadID  string      `json:"threadId"`
	Payload   any         `json:"payload"`
}
