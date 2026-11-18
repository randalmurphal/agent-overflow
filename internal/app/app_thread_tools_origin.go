package app

import (
	"log"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/usermessage"
)

// Who a thread request is from: the origin a request is accepted for, and
// the footer and attribution its message carries to the receiving thread.

// threadRequestOrigin is who a request is being accepted for.
//
// A local request's caller thread lives here, so everything about it is
// readable from the store. A request a paired computer forwarded has no
// row here at all, so the three things this computer would otherwise read
// from the caller travel with the call: its identity, its latest user
// message for the footer, and the authenticated device that owns the
// receipt.
type threadRequestOrigin struct {
	tracksSpawn bool
	caller      threadtools.Caller
	// foreign is true when the caller thread lives on another computer.
	foreign bool
	// ownerDevice authorizes the receipt: the authenticated device of the
	// calling computer, or threadReceiptLocalOwner for an in-process call.
	ownerDevice string
	// userMessage is the caller thread's latest message no agent wrote.
	// Read from the store for a local caller, carried for a foreign one.
	userMessage string
	// inherit is what a spawn takes from the calling thread where the call
	// overrides nothing. Project and checkout are not in it: they are
	// registered per computer and a spawn elsewhere names its own.
	inherit threadtools.SpawnDefaults
	// agent is set when the caller is a standing agent: read from the
	// store for a local caller, carried for a foreign one.
	agent *threadtools.SenderAgent
}

// localOrigin is the origin of a request one of this computer's own
// threads made. The caller thread is read back so a request is never
// accepted for a thread that has gone, and so a spawn inherits its
// provider settings from the row rather than from the tool call.
func (t threadToolsApp) localOrigin(caller threadtools.Caller) (threadRequestOrigin, error) {
	thread, err := t.localThread(caller.ThreadID)
	if err != nil {
		return threadRequestOrigin{}, err
	}
	agent, err := t.senderAgent(caller.ThreadID)
	if err != nil {
		return threadRequestOrigin{}, err
	}
	return threadRequestOrigin{
		caller:      caller,
		agent:       agent,
		ownerDevice: threadReceiptLocalOwner,
		inherit: threadtools.SpawnDefaults{
			Provider:    thread.Provider,
			Model:       thread.Model,
			Effort:      thread.ReasoningEffort,
			Mode:        thread.Mode,
			RuntimeMode: thread.RuntimeMode,
		},
	}, nil
}

// senderAgent names a thread of this computer as a standing agent, with the
// role line of its instructions, or returns nil for any other thread. A role
// that cannot be read is logged and left out: the message still names the
// sender as an agent.
func (t threadToolsApp) senderAgent(threadID string) (*threadtools.SenderAgent, error) {
	row, found, err := t.app.store.GetStandingAgent(threadID)
	if err != nil || !found {
		return nil, err
	}
	agent := &threadtools.SenderAgent{}
	if row.Home != "" {
		role, err := t.app.assistantRoles().Role(row.Home)
		if err != nil {
			log.Printf("thread tools: read the role line of standing agent %s from %s: %v", threadID, row.Home, err)
		}
		agent.Role = role
	}
	return agent, nil
}

// answerAgent is senderAgent for the thread that answers request token,
// read when the answer is written so the wake names the agent as it was then,
// from either computer. A store read that fails is logged and the answer is
// settled as an ordinary thread's: the answer matters more than its label,
// and a settlement is not retried for one.
func (t threadToolsApp) answerAgent(token, threadID string) *store.ThreadAnswerAgent {
	if threadID == "" {
		return nil
	}
	agent, err := t.senderAgent(threadID)
	if err != nil {
		log.Printf("thread tools: request %s: read whether %s is a standing agent; the answer records none: %v", token, threadID, err)
		return nil
	}
	if agent == nil {
		return nil
	}
	return &store.ThreadAnswerAgent{Role: agent.Role}
}

// requestMessage is the message the target thread reads: the sender's
// text, then the fixed footer, attributed to the sender with the facts the
// footer states. The footer is never hand-written here; the model reads the
// template and the timeline its facts, both from Footer.Message.
func (t threadToolsApp) requestMessage(
	origin threadRequestOrigin, token, kind, targetThreadID, text string, wantsAnswer bool,
) (usermessage.AgentMessage, error) {
	caller := origin.caller
	latest := origin.userMessage
	if !origin.foreign {
		var err error
		latest, _, err = t.app.store.LatestHumanUserText(caller.ThreadID)
		if err != nil {
			return usermessage.AgentMessage{}, err
		}
	}
	footer := threadtools.Footer{
		Title:    caller.Title,
		ThreadID: caller.ThreadID,
		// Named only for a sender on another computer: a local sender is on
		// the receiver's own computer and naming it would read as a second
		// machine. Either way the responder can reach it, because the
		// pairing the request arrived over is the route back.
		Computer:        t.senderComputerName(origin),
		Token:           token,
		AnswerRequested: wantsAnswer,
		SenderReachable: t.senderReachable(origin),
		UserMessage:     latest,
		Agent:           origin.agent,
	}
	// A worker's questions and approvals reach the thread that spawned it
	// (store.SpawnOwnerOf): one on this computer, or the thread on a paired
	// computer whose spawn's link owns it, through that link's pairing.
	owner, found, err := t.app.store.SpawnOwnerOf(targetThreadID)
	if err != nil {
		return usermessage.AgentMessage{}, err
	}
	if !origin.foreign {
		footer.RoutesPrompts = found && owner.ComputerID == "" && owner.ThreadID == caller.ThreadID
	} else {
		footer.RoutesPrompts = found && owner.ComputerID == caller.ComputerID && owner.ThreadID == caller.ThreadID &&
			owner.DeviceID == origin.ownerDevice
	}
	// The body is the sender's text and the footer is this computer's; a
	// body line that opens like the footer is quoted so only the footer
	// reads as one.
	return usermessage.Request(kind, usermessage.Sender{
		ComputerID: caller.ComputerID, ComputerName: caller.ComputerName, ThreadID: caller.ThreadID, Title: caller.Title,
	}, footer, text)
}

// senderComputerName is what the footer calls the sender's computer: what
// THIS computer calls the pairing, not what the sender calls itself, so the
// responder reads a name its own thread_options lists.
func (t threadToolsApp) senderComputerName(origin threadRequestOrigin) string {
	if !origin.foreign {
		return ""
	}
	name, _ := t.threadToolsBackendName(origin.caller.ComputerID)
	if name != "" {
		return name
	}
	return origin.caller.ComputerName
}

// senderReachable reports whether the responder can reach the sender's
// computer, which is what the footer's return-navigation lines depend on.
//
// Pairing is directional: a request arrives over the SENDER's credential
// for this computer, which says nothing about this computer holding one for
// it. A sender on this computer is always reachable; a foreign one only
// when this computer's own pairing set names it.
func (t threadToolsApp) senderReachable(origin threadRequestOrigin) bool {
	if !origin.foreign {
		return true
	}
	_, paired := t.threadToolsPairing(origin.caller.ComputerID)
	return paired
}
