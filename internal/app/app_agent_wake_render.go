package app

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"unicode/utf8"

	"agent-overflow/internal/agentmeta"
	"agent-overflow/internal/store"
	"agent-overflow/internal/untrustedtext"
	"agent-overflow/internal/usermessage"
)

// renderAgentLoopWake renders one loop fire, headed in its row by its note.
func renderAgentLoopWake(row store.AgentWake, fire agentWakeFire) (usermessage.AgentMessage, error) {
	var out strings.Builder
	writeAgentWakeFireLines(&out, fire)
	out.WriteString("note: ")
	out.WriteString(untrustedtext.Quote(row.Note, 0))
	return usermessage.Report(agentmeta.ReportLoop, usermessage.Reporter{Label: row.Label},
		fmt.Sprintf("Loop %s fired. %s %s", untrustedtext.Quote(row.Label, 0), agentWakeDataNotice, agentWakeUnattendedNotice), out.String(), row.Note)
}

// renderAgentWatchWake renders one watch wake: its stdout, then its stderr
// tail, together capped at agentWakeOutputCap with what was dropped stated,
// headed in its row by the first line it printed. The row is read as it
// stands now: when a wake of it did not reach the agent and none has said so
// since, this one says where that wake's output is kept.
func renderAgentWatchWake(row store.AgentWake, fire agentWakeFire, stdout, stderr cappedOutput) (usermessage.AgentMessage, error) {
	stdout = stdout.clip(agentWakeOutputCap - len(stderr.data))
	var out strings.Builder
	writeAgentWakeFireLines(&out, fire)
	if row.LastOutput != "" && row.Undelivered != "" {
		out.WriteString("The previous wake of this watch did not reach you; thread_status shows its output as last_output.\n")
	}
	out.WriteString("stdout:\n")
	writeQuotedLines(&out, stdout)
	if stderr.total > 0 {
		out.WriteString("stderr:\n")
		writeQuotedLines(&out, stderr)
	}
	headline := firstDataLine(string(stdout.data))
	if headline == "" {
		headline = firstDataLine(string(stderr.data))
	}
	return usermessage.Report(agentmeta.ReportWatch, usermessage.Reporter{Label: row.Label},
		fmt.Sprintf("Watch %s reported. %s %s", untrustedtext.Quote(row.Label, 0), agentWakeDataNotice, agentWakeUnattendedNotice),
		strings.TrimRight(out.String(), "\n"), headline)
}

// renderAgentSendWake renders a message an outside script sent with
// `agent-overflow thread send`, headed in its row by its first line.
func renderAgentSendWake(text string) (usermessage.AgentMessage, error) {
	capped := newCappedText(text, agentWakeOutputCap)
	var out strings.Builder
	writeQuotedLines(&out, capped)
	return usermessage.Report(agentmeta.ReportSend, usermessage.Reporter{},
		agentSendWakeHeader+" "+agentWakeDataNotice, strings.TrimRight(out.String(), "\n"), firstDataLine(string(capped.data)))
}

// firstDataLine is the first line of data that says something, the line a
// wake's row is headed by.
func firstDataLine(data string) string {
	for line := range strings.SplitSeq(data, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// queuedAgentMessage is the message a queue row holds, with the attribution
// and facts its payload kept (usermessage.DecodeStored): what a wake taken
// back or moved is queued again with. A payload that does not decode leaves
// the text alone with no attribution, and one whose attribution does not
// hold keeps the text, delivered as written, with facts that claim nothing
// of it.
func queuedAgentMessage(item store.FlushQueueItem) usermessage.AgentMessage {
	var payload flushQueuePayload
	if len(item.Payload) > 0 {
		if err := json.Unmarshal(item.Payload, &payload); err != nil {
			log.Printf("agent wakes: decode the payload of queued %s: %v", item.SendID, err)
		}
	}
	return storedAgentMessage(item.SendID, item.Message, payload.Origin, payload.OriginThread, payload.OriginWake)
}

// storedAgentMessage is usermessage.DecodeStored of the message sendID
// names, its refusal logged: the message it returns is delivered either way.
func storedAgentMessage(sendID, text, origin string, thread *agentmeta.OriginThread, wake *agentmeta.OriginWake) usermessage.AgentMessage {
	message, err := usermessage.DecodeStored(text, origin, thread, wake)
	if err != nil {
		log.Printf("agent wakes: read back %s: %v", sendID, err)
	}
	return message
}

// writeAgentWakeFireLines states what a fire stands for besides itself.
func writeAgentWakeFireLines(out *strings.Builder, fire agentWakeFire) {
	if fire.rerun {
		out.WriteString("This is a rerun: quitting the app cancelled the previous run before it finished.\n")
	}
	switch {
	case fire.missed > 0 && fire.closed:
		fmt.Fprintf(out, "This fire stands in for the ones it missed: missed %d while the app was closed.\n", fire.missed)
	case fire.missed > 0:
		fmt.Fprintf(out, "This fire stands in for the ones it missed: missed %d since the previous wake.\n", fire.missed)
	}
}

// writeQuotedLines writes one quoted line per line of output, then what the
// cap dropped.
func writeQuotedLines(out *strings.Builder, output cappedOutput) {
	text := strings.TrimSuffix(string(output.data), "\n")
	if text != "" || output.dropped() == 0 {
		for _, line := range strings.Split(text, "\n") {
			out.WriteString(untrustedtext.Quote(line, 0))
			out.WriteByte('\n')
		}
	}
	if dropped := output.dropped(); dropped > 0 {
		fmt.Fprintf(out, "[%d bytes dropped: a wake carries at most %d KiB]\n", dropped, agentWakeOutputCap>>10)
	}
}

// cappedOutput is a stream kept to a cap, with the stream's total length.
type cappedOutput struct {
	data  []byte
	total int64
}

func (o cappedOutput) dropped() int64 { return o.total - int64(len(o.data)) }

// clip keeps at most limit bytes of the head, cut on a rune boundary.
func (o cappedOutput) clip(limit int) cappedOutput {
	if limit < 0 {
		limit = 0
	}
	if len(o.data) <= limit {
		return o
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(o.data[cut]) {
		cut--
	}
	return cappedOutput{data: o.data[:cut], total: o.total}
}

func newCappedText(text string, limit int) cappedOutput {
	return cappedOutput{data: []byte(text), total: int64(len(text))}.clip(limit)
}

// outputBuffer collects a stream up to a cap, keeping its head or its tail.
// Safe for the copy goroutine exec runs and the reader at once.
type outputBuffer struct {
	mu    sync.Mutex
	limit int
	tail  bool
	out   cappedOutput
}

func newHeadOutput(limit int) *outputBuffer { return &outputBuffer{limit: limit} }
func newTailOutput(limit int) *outputBuffer { return &outputBuffer{limit: limit, tail: true} }

func (b *outputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.append(p)
	return len(p), nil
}

func (b *outputBuffer) append(p []byte) {
	b.out.total += int64(len(p))
	if !b.tail {
		if room := b.limit - len(b.out.data); room > 0 {
			b.out.data = append(b.out.data, p[:min(room, len(p))]...)
		}
		return
	}
	b.out.data = append(b.out.data, p...)
	if over := len(b.out.data) - b.limit; over > 0 {
		b.out.data = append(b.out.data[:0:0], b.out.data[over:]...)
		for len(b.out.data) > 0 && !utf8.RuneStart(b.out.data[0]) {
			b.out.data = b.out.data[1:]
		}
	}
}

// writeLine adds one long-running watch line, counting what the line itself
// lost to the cap.
func (b *outputBuffer) writeLine(line agentWatchLine) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.append(append(line.data, '\n'))
	b.out.total += line.dropped
}

// pending reports whether anything was written since the last take.
func (b *outputBuffer) pending() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.out.total > 0
}

// peek returns what the buffer holds without emptying it.
func (b *outputBuffer) peek() cappedOutput {
	b.mu.Lock()
	defer b.mu.Unlock()
	return cappedOutput{data: append([]byte(nil), b.out.data...), total: b.out.total}
}

// take returns what the buffer holds and empties it.
func (b *outputBuffer) take() cappedOutput {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.out
	b.out = cappedOutput{}
	return out
}

// restore puts back what take returned, ahead of anything written since.
func (b *outputBuffer) restore(taken cappedOutput) {
	b.mu.Lock()
	defer b.mu.Unlock()
	since := b.out
	b.out = cappedOutput{}
	b.append(taken.data)
	b.out.total = taken.total
	b.append(since.data)
	b.out.total = taken.total + since.total
}

func clipUTF8(text string, limit int) string {
	return string(newCappedText(text, limit).data)
}

// keptAgentWakeOutput is an undelivered wake's body as its row keeps it: cut
// to agentWakeOutputCap with what it dropped stated.
func keptAgentWakeOutput(body string) string {
	if len(body) <= agentWakeOutputCap {
		return body
	}
	// The drop line is under 80 bytes for any body length.
	kept := clipUTF8(body, agentWakeOutputCap-80)
	return kept + fmt.Sprintf("\n[%d bytes dropped: a kept wake is at most %d KiB]", len(body)-len(kept), agentWakeOutputCap>>10)
}
