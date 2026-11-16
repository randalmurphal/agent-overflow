// Package agentmeta owns how a user row's message is attributed to an
// agent or to the app, and how a person reads such a row: the attribution
// a row's meta stores (Meta, OriginThread, OriginWake, JoinedPart), the
// report kinds (reportKinds), the one reading of an agent's row (Describe,
// Project) and the sender line the model reads (WireText).
//
// It depends on no store, so the store derives what an agent's row shows
// (its find text) from the same rule the timeline renders it by.
// internal/usermessage owns the rest of the row's meta and writes the
// messages this package reads.
package agentmeta

import "agent-overflow/internal/threadtools"

// Meta is the part of a user row's meta its attribution is read from: the
// fields of usermessage.Meta of the same names and JSON keys
// (usermessage.Meta.Agent). A row's meta decodes into it directly.
type Meta struct {
	// ExpandComposerCommands marks a row that came through the composer:
	// the person's, whatever else it holds (IsAgentMessage).
	ExpandComposerCommands bool   `json:"expandComposerCommands,omitempty"`
	SendID                 string `json:"sendId,omitempty"`
	// Origin names the non-human author of a user row, in the same field
	// and vocabulary the provider-written rows use (`external-queue`,
	// `peer-session`). Agent thread tools write OriginAgentThread; the
	// frontend's one attribution branch labels every origin.
	Origin string `json:"origin,omitempty"`
	// OriginThread names the thread that wrote this row, on an
	// `agent-thread` row. Absent on every other origin.
	OriginThread *OriginThread `json:"originThread,omitempty"`
	// OriginWake names the loop, watch or outside send that wrote this row,
	// on an `agent-wake` row. Absent on every other origin.
	OriginWake *OriginWake `json:"originWake,omitempty"`
	// JoinedParts are a joined row's members as they appear in its text,
	// each with its attribution (JoinedPart).
	JoinedParts []JoinedPart `json:"joinedParts,omitempty"`
}

// OriginAgentThread is the Origin value of a row one thread's agent wrote
// into another thread through the thread tools.
const OriginAgentThread = "agent-thread"

// OriginAgentWake is the Origin value of a row the wake supervisor wrote: a
// loop's fire, a watch's output, an outside `agent-overflow thread send`, or
// a worker's prompt put to its agent.
// The row is data delivered to the thread, never the user speaking.
const OriginAgentWake = "agent-wake"

// OriginApp is the Origin value of a row Agent Overflow wrote in its own voice.
const OriginApp = "app"

// OriginWake identifies what wrote an `agent-wake` row: what its badge
// renders.
type OriginWake struct {
	// Kind is the report's ReportKind.
	Kind string `json:"kind"`
	// Label is the loop or watch label, the worker's title, the remote
	// job's label or the workflow's id; empty for an outside send.
	Label string `json:"label,omitempty"`
	// ThreadID is the worker of a `question` or `approval` wake.
	ThreadID string `json:"threadId,omitempty"`
	// Computer names the computer a `completion`'s job ran on.
	Computer string `json:"computer,omitempty"`
	// Facts is where the message's data is and what its row's header
	// says, written with the text (usermessage.Report).
	Facts *threadtools.MessageFacts `json:"facts,omitempty"`
	// Report is what the report's kind means to a person's view of it,
	// set only where a client receives the attribution (Served), never
	// stored.
	Report *ReportDescriptor `json:"report,omitempty"`
}

// OriginThread identifies the sending thread of an `agent-thread` row: what
// the row's header names, and the request token that keeps three requests
// from one thread to another distinguishable.
type OriginThread struct {
	ComputerID   string `json:"computerId,omitempty"`
	ComputerName string `json:"computerName,omitempty"`
	ThreadID     string `json:"threadId"`
	Title        string `json:"title,omitempty"`
	Token        string `json:"token,omitempty"`
	// Kind is what the row delivers: the request kind (`spawn`, `send`,
	// `ask`) on a request, or OriginKindWake on the wake carrying a
	// request's answer back. Absent on rows written before it existed;
	// readers fall back to the row's send id.
	Kind string `json:"kind,omitempty"`
	// Facts is what the message says, written with its text (the
	// usermessage constructors): a request's footer, an answer's outcome
	// and the threads it speaks for. The timeline renders the row from
	// these. Absent on rows written before they existed, whose text main's
	// templates wrote and readers parse.
	Facts *threadtools.MessageFacts `json:"facts,omitempty"`
}

// OriginKindWake is OriginThread.Kind on a wake. Its facts say which
// settlement it carries.
const OriginKindWake = "wake"

// IsAgentMessage reports whether a user row is a message an agent wrote, not
// the person: an `agent-thread` or `agent-wake` row. Such a row is no tick in
// the message navigator, no entry in the composer's recall and no queued
// message of the person's.
//
// A row that also carries ExpandComposerCommands is the person's: headless
// Claude joins queued messages at a turn boundary, so one row can hold the
// person's composer message after an agent's, and usermessage.JoinMetas
// attributes it to the agent. Only the composer path sets the flag. The SQL
// twin is store.AgentMessageFilterFor and the frontend's is
// isAgentMessageMeta.
func IsAgentMessage(meta Meta) bool {
	if meta.ExpandComposerCommands {
		return false
	}
	return meta.Origin == OriginAgentThread || meta.Origin == OriginAgentWake
}

// Clone is a copy of t that shares nothing with it; nil for nil.
func (t *OriginThread) Clone() *OriginThread {
	if t == nil {
		return nil
	}
	copied := *t
	copied.Facts = cloneFacts(t.Facts)
	return &copied
}

// Clone is a copy of w that shares nothing with it; nil for nil.
func (w *OriginWake) Clone() *OriginWake {
	if w == nil {
		return nil
	}
	copied := *w
	copied.Facts = cloneFacts(w.Facts)
	if w.Report != nil {
		report := *w.Report
		copied.Report = &report
	}
	return &copied
}

func cloneFacts(facts *threadtools.MessageFacts) *threadtools.MessageFacts {
	if facts == nil {
		return nil
	}
	copied := facts.Clone()
	return &copied
}
