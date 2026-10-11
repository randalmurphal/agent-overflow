package claude

import (
	"encoding/json"
	"log"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// CronCreate / CronList / CronDelete manage jobs the CLI holds in its own
// process and fires as turns it starts itself (claude-wire.md §E11). No
// task lifecycle stands behind a job, so this parser keeps the table of
// armed jobs and emits EventScheduledJobs, the full list with each job's
// latest possible next fire, whenever it changes. The table dies with the
// Parser, which lives exactly as long as its CLI process; that is also
// the job's lifetime, `durable` or not, as far as this session goes.
//
// The tool names classify the acks; each ack's shape is checked before it
// changes the table, because a renamed or reshaped tool must not release
// a job that is still armed.

const (
	cronCreateToolName = "CronCreate"
	cronDeleteToolName = "CronDelete"
	cronListToolName   = "CronList"
)

// maxTrackedCronJobs bounds the job table above the CLI's own limit of 50
// armed jobs (2.1.284 refuses a 51st CronCreate), so the cap is never the
// reason a job goes untracked.
const maxTrackedCronJobs = 64

// cronToolUse is the assistant-side record of an in-flight cron tool call.
// Only CronCreate needs its input: the ack carries no cron expression.
type cronToolUse struct {
	name string
	cron string
}

// cronJob is one armed job. match is the next cron match after the job's
// anchor (its creation, then its latest fire); fireBy bounds when the CLI
// fires for that match.
type cronJob struct {
	id        string
	cron      string
	schedule  cronSchedule
	recurring bool
	match     time.Time
	fireBy    time.Time
}

func isCronToolName(name string) bool {
	switch name {
	case cronCreateToolName, cronDeleteToolName, cronListToolName:
		return true
	default:
		return false
	}
}

func (p *Parser) markCronTool(toolUseID, name string, input json.RawMessage) {
	if toolUseID == "" {
		return
	}
	use := cronToolUse{name: name}
	if name == cronCreateToolName {
		var decoded struct {
			Cron string `json:"cron"`
		}
		if len(input) > 0 && json.Unmarshal(input, &decoded) == nil {
			use.cron = strings.TrimSpace(decoded.Cron)
		}
	}
	if p.cronToolUses == nil || len(p.cronToolUses) >= parserTaskMapCap {
		p.cronToolUses = make(map[string]cronToolUse)
	}
	p.cronToolUses[toolUseID] = use
}

// takeCronTool releases the record for toolUseID: a result is a one-shot.
func (p *Parser) takeCronTool(toolUseID string) (cronToolUse, bool) {
	if toolUseID == "" || p.cronToolUses == nil {
		return cronToolUse{}, false
	}
	use, ok := p.cronToolUses[toolUseID]
	if ok {
		delete(p.cronToolUses, toolUseID)
	}
	return use, ok
}

// cronListedJob is one entry of CronList's `jobs`.
type cronListedJob struct {
	ID        string `json:"id"`
	Cron      string `json:"cron"`
	Recurring *bool  `json:"recurring"`
}

// appendCronAckEvents applies a cron tool's ack to the job table and
// emits the new snapshot when the table changed. A refused call
// (`is_error`, such as CronDelete of an unknown id) changed nothing in
// the CLI either.
func (p *Parser) appendCronAckEvents(
	events []provider.ProviderEvent,
	threadID string,
	now time.Time,
	toolUseID string,
	use cronToolUse,
	block map[string]json.RawMessage,
	toolUseResultRaw json.RawMessage,
) []provider.ProviderEvent {
	var isError bool
	if v, ok := block["is_error"]; ok {
		_ = json.Unmarshal(v, &isError)
	}
	if isError || len(toolUseResultRaw) == 0 {
		return events
	}
	var changed bool
	var problem string
	switch use.name {
	case cronCreateToolName:
		var ack struct {
			ID        string `json:"id"`
			Recurring *bool  `json:"recurring"`
		}
		if json.Unmarshal(toolUseResultRaw, &ack) != nil || strings.TrimSpace(ack.ID) == "" || ack.Recurring == nil {
			return events
		}
		changed, problem = p.armCronJob(strings.TrimSpace(ack.ID), use.cron, *ack.Recurring, now)
	case cronDeleteToolName:
		var ack struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(toolUseResultRaw, &ack) != nil || strings.TrimSpace(ack.ID) == "" {
			return events
		}
		id := strings.TrimSpace(ack.ID)
		if _, ok := p.cronJobs[id]; ok {
			delete(p.cronJobs, id)
			changed = true
		}
	case cronListToolName:
		var ack struct {
			Jobs *[]cronListedJob `json:"jobs"`
		}
		if json.Unmarshal(toolUseResultRaw, &ack) != nil || ack.Jobs == nil {
			return events
		}
		changed, problem = p.reconcileCronJobs(*ack.Jobs, now)
	}
	if problem != "" {
		events = append(events, provider.ProviderEvent{
			Kind:      provider.EventError,
			ThreadID:  threadID,
			ItemID:    toolUseID,
			Content:   problem,
			Timestamp: now,
		})
	}
	if !changed {
		return events
	}
	return p.appendScheduledJobsSnapshot(events, threadID, now)
}

// armCronJob records a job the CLI accepted. The problem string is set
// when AO cannot compute the job's fire time: the CLI accepted an
// expression this parser does not read, so the idle reaper cannot see
// the job, and the user is told so rather than finding the session closed.
func (p *Parser) armCronJob(id, cron string, recurring bool, anchor time.Time) (bool, string) {
	schedule, ok := parseCronSchedule(cron)
	if !ok {
		return false, "Claude scheduled job " + id + " with cron expression \"" + cron + "\", which Agent Overflow cannot read. The session may be closed while idle before the job fires."
	}
	job := cronJob{id: id, cron: cron, schedule: schedule, recurring: recurring}
	if !job.anchor(anchor) {
		return false, ""
	}
	if _, known := p.cronJobs[id]; !known && len(p.cronJobs) >= maxTrackedCronJobs {
		log.Printf("claude: cron job table full (%d); job %s is not tracked", maxTrackedCronJobs, id)
		return false, ""
	}
	if p.cronJobs == nil {
		p.cronJobs = make(map[string]cronJob)
	}
	p.cronJobs[id] = job
	return true, ""
}

// reconcileCronJobs makes the table match a CronList snapshot: a listed
// job the table lacks is anchored at now (an upper bound, since its real
// anchor is earlier), and a tracked job the list omits is gone.
func (p *Parser) reconcileCronJobs(listed []cronListedJob, now time.Time) (bool, string) {
	changed := false
	present := make(map[string]struct{}, len(listed))
	var problems []string
	for _, entry := range listed {
		id := strings.TrimSpace(entry.ID)
		if id == "" || entry.Recurring == nil {
			continue
		}
		present[id] = struct{}{}
		if _, known := p.cronJobs[id]; known {
			continue
		}
		added, problem := p.armCronJob(id, strings.TrimSpace(entry.Cron), *entry.Recurring, now)
		changed = changed || added
		if problem != "" {
			problems = append(problems, problem)
		}
	}
	for id := range p.cronJobs {
		if _, ok := present[id]; !ok {
			delete(p.cronJobs, id)
			changed = true
		}
	}
	return changed, strings.Join(problems, " ")
}

// noteScheduledFire moves the table past a fire the CLI just started at
// `at`. The fire's transcript row names no job, so every job whose match
// has come is treated as fired: a recurring one re-anchors at `at`, which
// never moves its deadline earlier, and a one-shot is released. A
// one-shot the CLI fired early (before its match) stays until its match
// passes, which only keeps the session a little longer.
func (p *Parser) noteScheduledFire(events []provider.ProviderEvent, threadID string, at time.Time) []provider.ProviderEvent {
	changed := false
	for id, job := range p.cronJobs {
		if job.match.After(at) {
			continue
		}
		changed = true
		if !job.recurring || !job.anchor(at) {
			delete(p.cronJobs, id)
			continue
		}
		p.cronJobs[id] = job
	}
	if !changed {
		return events
	}
	return p.appendScheduledJobsSnapshot(events, threadID, at)
}

func (j *cronJob) anchor(at time.Time) bool {
	match, ok := j.schedule.next(at)
	if !ok {
		return false
	}
	fireBy, ok := j.schedule.fireBy(at, j.recurring)
	if !ok {
		return false
	}
	j.match, j.fireBy = match, fireBy
	return true
}

func (p *Parser) appendScheduledJobsSnapshot(events []provider.ProviderEvent, threadID string, now time.Time) []provider.ProviderEvent {
	jobs := make([]provider.ScheduledJob, 0, len(p.cronJobs))
	for _, job := range p.cronJobs {
		jobs = append(jobs, provider.ScheduledJob{
			ID:           job.id,
			Cron:         job.cron,
			Recurring:    job.recurring,
			FireByUnixMs: job.fireBy.UnixMilli(),
		})
	}
	slices.SortFunc(jobs, func(a, b provider.ScheduledJob) int { return strings.Compare(a.ID, b.ID) })
	meta, err := json.Marshal(provider.ScheduledJobsMeta{Jobs: jobs})
	if err != nil {
		log.Printf("claude: marshal scheduled jobs for %s: %v", threadID, err)
		return events
	}
	return append(events, provider.ProviderEvent{
		Kind:      provider.EventScheduledJobs,
		ThreadID:  threadID,
		Meta:      meta,
		Timestamp: now,
	})
}
