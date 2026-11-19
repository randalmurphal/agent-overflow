package agentmeta

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// ReportKind is the kind of a report (usermessage.Report), the OriginWake
// kind its row is badged with: one of the Report* constants, each defined
// once in reportKinds with everything it means to the readers of its
// message. usermessage.Report writes no other and usermessage.DecodeStored
// reads no other.
type ReportKind string

// The kinds of report.
const (
	// ReportLoop and ReportWatch are a loop's fire and a watch's output,
	// and the kinds of the stored loops and watches (store.AgentWakeLoop,
	// store.AgentWakeWatch).
	ReportLoop  ReportKind = "loop"
	ReportWatch ReportKind = "watch"
	// ReportSend is an outside `agent-overflow thread send`.
	ReportSend ReportKind = "send"
	// ReportQuestion and ReportApproval are a worker's prompt put to the
	// thread that spawned it.
	ReportQuestion ReportKind = "question"
	ReportApproval ReportKind = "approval"
	// ReportCompletion is a remote job's report.
	ReportCompletion ReportKind = "completion"
	// ReportWorkflow is a workflow run's wake.
	ReportWorkflow ReportKind = "workflow"
	// ReportOpening and ReportHandOff are the messages that open an
	// assistant's first thread and each later one.
	ReportOpening ReportKind = "assistant"
	ReportHandOff ReportKind = "handoff"
)

// reportKind is what a ReportKind means to each reader of a report: the
// person's view of its row (ReportDescriptor), thread_show
// (OriginWake.TranscriptName) and the sender line the agent reads
// (WireText). Each name is resolved from the report's attribution.
type reportKind struct {
	// category, label and title are the row's kind, gutter label and kind
	// in words (ReportDescriptor).
	category, label, title string
	// badge is the kind's word on a badge, before the reporter's label.
	badge string
	// data is set when the report's data is quoted for the agent, which the
	// row decodes; otherwise the row shows it as written.
	data bool
	// sender names the reporter in the row's header: its label, else the
	// kind's title, when nil.
	sender func(OriginWake) string
	// transcript names it in thread_show: the kind and the quoted label,
	// when nil.
	transcript func(OriginWake) string
	// wire names it on the sender line the agent reads; nil for the app
	// itself.
	wire func(OriginWake) string
}

// reportKinds are every ReportKind and what it means.
var reportKinds = map[ReportKind]reportKind{
	ReportLoop: {
		category: "loop", label: "Loop", title: "Loop wake", badge: "Loop", data: true,
		wire: func(w OriginWake) string { return "your loop " + quoteName(w.Label) },
	},
	ReportWatch: {
		category: "watch", label: "Watch", title: "Watch wake", badge: "Watch", data: true,
		wire: func(w OriginWake) string { return "your watch " + quoteName(w.Label) },
	},
	ReportSend: {
		category: "script", label: "Shell", title: "Message from a script", badge: "Sent by a script", data: true,
		sender:     func(OriginWake) string { return "A script" },
		transcript: func(OriginWake) string { return "a script (agent-overflow thread send)" },
		wire:       func(OriginWake) string { return "a script, through agent-overflow thread send" },
	},
	ReportQuestion: {
		category: "worker-question", label: "Ask", title: "Worker's question", badge: "Question", data: true,
		transcript: workerTranscriptName, wire: workerWireName,
	},
	ReportApproval: {
		category: "worker-approval", label: "Allow", title: "Worker's approval request", badge: "Approval", data: true,
		transcript: workerTranscriptName, wire: workerWireName,
	},
	ReportCompletion: {
		// A job's output is its own text, shown as written.
		category: "job", label: "Job", title: "Remote job finished", badge: "Remote job",
		sender: func(w OriginWake) string {
			if name := strings.Join(slices.DeleteFunc([]string{w.Label, w.Computer}, isEmpty), " on "); name != "" {
				return name
			}
			return "A remote job"
		},
		transcript: func(w OriginWake) string { return fmt.Sprintf("remote job %q on %s", w.Label, w.Computer) },
		wire: func(w OriginWake) string {
			job := "a remote job"
			if w.Label != "" {
				job = "the remote job " + quoteName(w.Label)
			}
			if w.Computer != "" {
				job += " on " + quoteName(w.Computer)
			}
			return job
		},
	},
	ReportWorkflow: {
		category: "workflow", label: "Run", title: "Workflow wake", badge: "Workflow", data: true,
		transcript: func(w OriginWake) string { return fmt.Sprintf("workflow %q", w.Label) },
		wire:       func(w OriginWake) string { return "the workflow run " + quoteName(w.Label) },
	},
	// An assistant's opening and handoff blocks are the app's own prose
	// (docs/specs/standing-agents.md, "Standing agent").
	ReportOpening: {
		category: "opening", label: "Start", title: "Assistant's opening message", badge: "Opening",
		sender: appSender,
	},
	ReportHandOff: {
		category: "handoff", label: "Moved", title: "New thread, same assistant", badge: "New thread",
		sender: appSender,
	},
}

func workerTranscriptName(w OriginWake) string {
	return fmt.Sprintf("%q (%s), its %s", w.Label, w.ThreadID, w.Kind)
}

func workerWireName(w OriginWake) string {
	return threadName(w.Label, w.ThreadID, "") + ", which you spawned"
}

func appSender(OriginWake) string { return "Agent Overflow" }

func isEmpty(value string) bool { return value == "" }

// ReportKinds are every ReportKind, in order.
func ReportKinds() []ReportKind { return slices.Sorted(maps.Keys(reportKinds)) }

// Known reports whether k is one of the ReportKinds.
func (k ReportKind) Known() bool {
	_, known := reportKinds[k]
	return known
}

// ReportDescriptor is a report's kind as a person's view of its row reads
// it, resolved for the reporter its attribution names. It is served on the
// attribution of every report a client receives (OriginWake.Report): rows
// as Describe reads them and queued messages. The view takes the report's
// kind, sender and body from it and keeps only icons and layout, so a kind
// added here renders without a change there. A report of a kind this
// computer does not know carries none, and its row is shown whole.
type ReportDescriptor struct {
	// Category is what kind of report the row is; it chooses the row's
	// icon, and a category the view has no icon for gets a default one.
	Category string `json:"category"`
	// Label is the row's gutter label, five characters at most; Title is
	// the kind in words.
	Label string `json:"label"`
	Title string `json:"title"`
	// Sender is who the row's header names as the reporter, and Opens the
	// thread that name opens, "" for none.
	Sender string `json:"sender"`
	Opens  string `json:"opens,omitempty"`
	// Badge names the report where it is a badge: a queued message, or a
	// person's row it was joined into.
	Badge string `json:"badge"`
	// Body is how the row shows the report's data: ReportBodyData or
	// ReportBodyText.
	Body string `json:"body"`
}

// How a row shows a report's data (ReportDescriptor.Body).
const (
	// ReportBodyData is data quoted for the agent, its escapes decoded for
	// the person (ReadableWakeText).
	ReportBodyData = "data"
	// ReportBodyText is text shown as written.
	ReportBodyText = "text"
)

// descriptor is w's ReportDescriptor, nil for a kind this computer does not
// know.
func (w *OriginWake) descriptor() *ReportDescriptor {
	if w == nil {
		return nil
	}
	kind, known := reportKinds[ReportKind(w.Kind)]
	if !known {
		return nil
	}
	described := ReportDescriptor{
		Category: kind.category, Label: kind.label, Title: kind.title, Opens: w.ThreadID,
		Sender: firstNonEmpty(w.Label, kind.title), Badge: kind.badge, Body: ReportBodyText,
	}
	if kind.sender != nil {
		described.Sender = kind.sender(*w)
	}
	if w.Label != "" {
		described.Badge += " · " + w.Label
	}
	if kind.data {
		described.Body = ReportBodyData
	}
	return &described
}

// Served is w as a client receives it: a copy carrying its kind's
// ReportDescriptor, which a stored attribution never does.
func (w *OriginWake) Served() *OriginWake {
	served := w.Clone()
	if served != nil {
		served.Report = w.descriptor()
	}
	return served
}

// TranscriptName is who wrote the report as thread_show names it to an
// agent.
func (w OriginWake) TranscriptName() string {
	if kind := reportKinds[ReportKind(w.Kind)]; kind.transcript != nil {
		return kind.transcript(w)
	}
	return fmt.Sprintf("%s %q", w.Kind, w.Label)
}

// wireName is who wrote the report as the sender line the agent reads
// names it, "" for the app itself.
func (w OriginWake) wireName() string {
	if kind := reportKinds[ReportKind(w.Kind)]; kind.wire != nil {
		return kind.wire(w)
	}
	return ""
}
