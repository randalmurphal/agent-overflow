package browser

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"agent-overflow/internal/browser/quota"
)

// Cookie jars. A jar is one engine profile: cookies and every other kind of
// site data the engine keeps. Which jar a thread's pages open in is the
// thread's jar setting (SessionInfo.Jar, saved with its pages in
// page_records.go):
//
//   - worktree (the default): one jar per canonical workspace, shared by the
//     threads on it. Its identity is the workspace path itself, so every
//     engine derives exactly the profile it derived before jars existed.
//   - project: one jar per project, shared by every thread of the project
//     that chose it, across its worktrees.
//   - thread: this thread only, persistent.
//   - private: this thread only, always ephemeral: never part of a
//     persistent profile, and discarded when its last page closes, after
//     which its next page gets a new identity, as does every generation of
//     the setting. Where an ephemeral profile lives until then is the
//     engine's (browser-tools.md, Platform behavior).
//
// The jar is only the engine's storage partition. Authority stays with the
// page's workspace: navigation and file permissions are the page's own
// Access (pageHooks.Allow on the in-process engines; on the CDP engines
// the profile's policy, which answers with the Access of the page a
// request's frame belongs to or its opener's: jarAllows), and download
// accounting is per workspace (workspaceAuthority). jarForLocked is the one place a thread and its
// setting become a jar identity; an engine receives only that identity and
// whether the jar is ephemeral (profileOptions).

// The jar kinds a thread's browser can use.
const (
	JarWorktree = "worktree"
	JarProject  = "project"
	JarThread   = "thread"
	JarPrivate  = "private"
)

// validJarKind reports whether kind names a jar kind.
func validJarKind(kind string) bool {
	switch kind {
	case JarWorktree, JarProject, JarThread, JarPrivate:
		return true
	}
	return false
}

// jarKindOf answers a session's jar kind; an unset one is the default.
func jarKindOf(info SessionInfo) string {
	if info.Jar == "" {
		return JarWorktree
	}
	return info.Jar
}

// jarIdentity is one jar as the Manager keys it and an engine receives it.
type jarIdentity struct {
	// key identifies the jar: the scope map's key and the engine's profile
	// identity (profileOptions.Jar). A worktree jar's key is the canonical
	// workspace; every other kind has a prefix no absolute path starts with.
	key       jarKey
	ephemeral bool
	// private is a private jar's instance token, and owner the thread
	// lifetime that holds it; "" and nil otherwise.
	private string
	owner   *threadBrowser
}

// jarScope is one open cookie jar: its engine profile and the pages in it,
// which may belong to several workspaces' authority.
type jarScope struct {
	jar     jarIdentity
	profile engineProfile
	pages   map[PageID]*managedPage
	// creating holds the pages being created in this scope, reserving it.
	// Guarded by m.mu.
	creating map[*managedPage]struct{}
	// closing holds the pages closed and not yet disposed, by page id
	// (lifecycle.go). They are out of pages, so nothing finds or presents
	// them as live, and still count toward every cap. Guarded by m.mu.
	closing map[PageID]*pageClose
	// downloadDir is where an engine keeping downloads as artifacts writes
	// this jar's downloads before they move to their workspace's directory;
	// empty for every other engine (downloadSaving).
	downloadDir string
}

// jarForLocked maps a page of the thread lifetime tb, with its caller
// access, and a jar kind to the jar the page opens in: a thread's jar is
// tb's thread's, whatever access names. access.Workspace is canonical. A private jar without a live instance takes a new token here.
// The caller holds m.mu.
func (m *Manager) jarForLocked(tb *threadBrowser, access Caller, kind string) (jarIdentity, error) {
	jar := jarIdentity{ephemeral: !m.config.PersistSiteData}
	switch kind {
	case JarWorktree:
		jar.key = jarKey(access.Workspace)
	case JarProject:
		if strings.TrimSpace(access.ProjectID) == "" {
			return jarIdentity{}, fmt.Errorf("browser: this thread has no project to share a cookie jar with")
		}
		jar.key = jarKey("project:" + access.ProjectID)
	case JarThread:
		jar.key = jarKey("thread:" + string(tb.id))
	case JarPrivate:
		if tb.session.privateJar == "" {
			tb.session.privateJar = uuid.NewString()
		}
		jar.key = jarKey("private:" + string(tb.id) + ":" + tb.session.privateJar)
		jar.private, jar.owner, jar.ephemeral = tb.session.privateJar, tb, true
	default:
		return jarIdentity{}, fmt.Errorf("browser: unknown cookie jar %q", kind)
	}
	return jar, nil
}

// jarDirName is the directory name an engine keeps a jar's data under and
// the jar's download directory: a digest, because a jar key can be long, can
// hold characters a directory name cannot, and must not be readable from a
// listing.
func jarDirName(jar jarKey) string {
	digest := sha256.Sum256([]byte(jar))
	return fmt.Sprintf("%x", digest[:12])
}

// jarCurrentLocked reports whether p was opened under its thread lifetime's
// current jar setting. A switch advances the generation, so a page opened before it
// is refused at admission (create, restore, popup) and nothing lands in the
// old jar. The caller holds m.mu.
func (m *Manager) jarCurrentLocked(p *managedPage) bool {
	return p.owner.session.jarGen == p.jarGen
}

// jarAllows is a jar profile's own navigation policy (profileOptions.Allow).
// A jar carries site data, never authority. owners is the request's frame
// and then the pages that opened its page, nearest first; the request is
// allowed what the first owner that resolves to a page of the jar may open
// (framePageLocked). So a popup the Manager has not adopted yet has its
// opener's authority, which is what it is allowed once adopted. A request
// none of whose owners resolves (a frame no page has reported yet, a popup
// seen before its opener is known or after its opener closed) is answered
// unresolved, with what a page with no workspace's authority may open: its
// engine allows that at once, and asks again for anything else
// (awaitRequestOwner).
func (m *Manager) jarAllows(scope *jarScope, owners []frameID, rawURL string) (allowed, resolved bool) {
	var access Caller
	m.mu.Lock()
	for _, owner := range owners {
		if p := framePageLocked(scope, owner); p != nil {
			access, resolved = p.access, true
			break
		}
	}
	m.mu.Unlock()
	return m.navigationAllowed(access, rawURL), resolved
}

// framePageLocked answers the page of scope, live or being created, that
// frame belongs to, or nil. A page's main frame is its handle. The caller
// holds m.mu.
func framePageLocked(scope *jarScope, frame frameID) *managedPage {
	if frame == "" {
		return nil
	}
	owns := func(p *managedPage) bool {
		return p.driver != nil && (frameID(p.driver.Handle()) == frame || p.driver.OwnsFrame(frame))
	}
	for _, p := range scope.pages {
		if owns(p) {
			return p
		}
	}
	for p := range scope.creating {
		if owns(p) {
			return p
		}
	}
	return nil
}

// workspaceAuthority is what a canonical workspace owns across every jar its
// pages are in: on an engine whose downloads AO keeps, the directory they
// land in and the bytes they reserve. It lives while the workspace has a
// page, live, closing or being created, in any jar (pruneAuthoritiesLocked).
// Its download bytes are the workspace's one pool (Manager.workspacePools),
// which a reservation against it keeps after the authority goes, so a
// workspace whose authority is made again meets the bytes still held.
type workspaceAuthority struct {
	workspace     string
	downloadDir   string
	downloadBytes *quota.Pool
	// pool is the authority's hold on downloadBytes, released as it goes
	// (pruneAuthoritiesLocked, forgetAuthoritiesLocked).
	pool *quota.PoolHold
}

// authorityLocked answers the workspace's authority, creating it. The caller
// holds m.mu.
func (m *Manager) authorityLocked(workspace, downloadDir string) *workspaceAuthority {
	if m.authorities == nil {
		m.authorities = make(map[string]*workspaceAuthority)
	}
	authority := m.authorities[workspace]
	if authority == nil {
		pool := m.workspacePools.Hold(workspace)
		authority = &workspaceAuthority{workspace: workspace, downloadDir: downloadDir, downloadBytes: pool.Pool(), pool: pool}
		m.authorities[workspace] = authority
	}
	return authority
}

// pruneAuthoritiesLocked forgets the authority of every workspace with no
// page left in any jar. The caller holds m.mu.
func (m *Manager) pruneAuthoritiesLocked() {
	if len(m.authorities) == 0 {
		return
	}
	used := make(map[string]bool, len(m.authorities))
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			used[p.access.Workspace] = true
		}
		for _, c := range scope.closing {
			used[c.p.access.Workspace] = true
		}
		for p := range scope.creating {
			used[p.access.Workspace] = true
		}
	}
	for workspace, authority := range m.authorities {
		if !used[workspace] {
			delete(m.authorities, workspace)
			authority.pool.Release()
		}
	}
}

// forgetAuthoritiesLocked forgets every workspace's authority, for a
// closing browser. The caller holds m.mu.
func (m *Manager) forgetAuthoritiesLocked() {
	for _, authority := range m.authorities {
		authority.pool.Release()
	}
	m.authorities = nil
}

// workspaceLoadLocked counts the engine pages, in every jar, whose authority
// is workspace: the quantity the per-workspace cap bounds, and of them the
// pages still closing. The caller holds m.mu.
func (m *Manager) workspaceLoadLocked(workspace string) (load, closing int) {
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.access.Workspace == workspace {
				load++
			}
		}
		for _, c := range scope.closing {
			if c.p.access.Workspace == workspace {
				load++
				closing++
			}
		}
		for p := range scope.creating {
			if p.access.Workspace == workspace {
				load++
			}
		}
	}
	return load, closing
}
