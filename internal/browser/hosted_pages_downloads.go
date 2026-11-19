package browser

import (
	"fmt"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/webview2host"
)

// A hosted page's downloads in its record (hosted_pages.go): each one's
// admission to the Manager, its cancels, its end, and the status loop's
// questions. Every function here is a hostedPages method, under its lock.

// hostedDownloadAdmission is where a download's admission to the Manager
// stands.
type hostedDownloadAdmission int

const (
	// hostedAnnouncing: its first snapshot was accepted and the Manager
	// has not admitted it yet.
	hostedAnnouncing hostedDownloadAdmission = iota + 1
	// hostedAnnouncingCanceled: the Manager canceled it while its
	// admission was in flight, its row already listed; the cancel goes to
	// the launcher as it is admitted.
	hostedAnnouncingCanceled
	// hostedAdmitted: the Manager has it, to cancel or to ask about.
	hostedAdmitted
)

// hostedDownload is one launcher download, from its first snapshot until
// it ends or its page's lane retires the page.
type hostedDownload struct {
	key hostedDownloadKey
	// Guarded by hostedPages.mu. heard is when the last snapshot arrived,
	// or the last status was asked; cancels is the last cancel attempt
	// sent, and reflected the latest a snapshot named: an attempt the
	// launcher has not named yet is sent again with each status question.
	heard              time.Time
	admission          hostedDownloadAdmission
	cancels, reflected webview2host.CancelAttempt
	// The lane's alone: applied is the number of the last snapshot
	// applied, and refused the cancel attempt whose refusal the Manager has.
	applied uint64
	refused webview2host.CancelAttempt
}

// live answers whether d still runs, as its lane sees it, with its
// admission and last cancel sent, and records the cancel attempt the
// snapshot being applied names.
func (p *hostedPages) live(d *hostedDownload, named webview2host.CancelAttempt) (live bool, admission hostedDownloadAdmission, cancels webview2host.CancelAttempt) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.records[d.key.page]
	if r == nil || r.downloads[d.key.id] != d {
		return false, 0, 0
	}
	d.reflected = max(d.reflected, named)
	return true, d.admission, d.cancels
}

// admit records the Manager's admission of d and answers whether it
// canceled d while that admission was in flight.
func (p *hostedPages) admit(d *hostedDownload) (canceled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	canceled = d.admission == hostedAnnouncingCanceled
	d.admission = hostedAdmitted
	return canceled
}

// end records d's end: it leaves its page's running downloads for its
// ended ones, the oldest past maxHostedEndedDownloads forgotten.
func (p *hostedPages) end(d *hostedDownload) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.records[d.key.page]
	if r == nil || r.downloads[d.key.id] != d {
		return
	}
	delete(r.downloads, d.key.id)
	r.ended = append(r.ended, d.key.id)
	if over := len(r.ended) - maxHostedEndedDownloads; over > 0 {
		for _, id := range r.ended[:over] {
			p.dropOwnerLocked(id, d.key.page)
		}
		r.ended = append([]downloadID(nil), r.ended[over:]...)
	}
}

// cancel takes the Manager's cancel of download id: one whose admission is
// in flight keeps it, and is answered not admitted; ok is false for a
// download that no longer runs, or whose page's close or discard was
// received, which ends it.
func (p *hostedPages) cancel(id downloadID) (d *hostedDownload, admitted, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	page, known := p.owners[id]
	if !known {
		return nil, false, false
	}
	r := p.records[page]
	if r == nil || r.downloads[id] == nil || !r.state.admits() {
		return nil, false, false
	}
	d = r.downloads[id]
	switch d.admission {
	case hostedAnnouncing:
		d.admission = hostedAnnouncingCanceled
		return d, false, true
	case hostedAnnouncingCanceled:
		return d, false, true
	case hostedAdmitted:
		return d, true, true
	default:
		panic(fmt.Sprintf("browser: hosted download admission %d", d.admission))
	}
}

// nextCancel numbers d's next cancel attempt, taken before it is sent so a
// refusal that arrives at once matches it.
func (p *hostedPages) nextCancel(d *hostedDownload) webview2host.CancelAttempt {
	p.mu.Lock()
	defer p.mu.Unlock()
	d.cancels++
	return d.cancels
}

// unsent gives back attempt, which could not be sent, so no status
// question makes it later.
func (p *hostedPages) unsent(d *hostedDownload, attempt webview2host.CancelAttempt) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d.cancels == attempt {
		d.cancels--
	}
}

// popupStart answers, for a download of page, the start naming the popup
// it began in, that popup's openers, nearest first, and its profile, when
// page is a popup the Manager has not listed; empty otherwise.
func (p *hostedPages) popupStart(page ids.PageHandle) downloadStart {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.records[page]
	if r == nil || !r.state.pendingPopup() {
		return downloadStart{}
	}
	start := downloadStart{Popup: page, Profile: r.profile}
	// Each opener is reported before its popups, so the chain ends; the
	// bound stops a cycle all the same.
	for opener := r.opener; opener != "" && len(start.Openers) <= len(p.records); {
		start.Openers = append(start.Openers, opener)
		next := p.records[opener]
		if next == nil || next.opener == "" {
			break
		}
		opener = next.opener
	}
	return start
}

// watchLocked starts the status loop unless it runs. The caller holds mu.
func (p *hostedPages) watchLocked() {
	if p.watching {
		return
	}
	p.watching = true
	if p.stop == nil {
		p.stop = make(chan struct{})
	}
	p.watch(p.stop)
}

// quiet answers the status questions of the loop stop belongs to: one for
// each admitted download not heard from for interval, or whose last cancel
// no snapshot named yet, each naming that cancel. keep is false once the
// loop is to end: Stop ended it, or no download runs.
func (p *hostedPages) quiet(stop <-chan struct{}, now time.Time, interval time.Duration) (ask []webview2host.Directive, keep bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stop == nil || stop != p.stop {
		return nil, false
	}
	running := 0
	for _, r := range p.records {
		for id, d := range r.downloads {
			running++
			if d.admission == hostedAdmitted && (now.Sub(d.heard) >= interval || d.cancels > d.reflected) {
				d.heard = now
				ask = append(ask, webview2host.Directive{Op: webview2host.OpDownloadStatus, PageID: string(d.key.page), Download: string(id), Cancel: d.cancels})
			}
		}
	}
	if running == 0 {
		p.watching = false
		return nil, false
	}
	return ask, true
}
