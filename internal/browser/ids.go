package browser

// The identifiers this package owns, one defined type per kind, so a value
// of one kind cannot be passed where another is meant. Each is converted
// from or to a plain string once, where it enters or leaves the package: a
// bound DTO, an MCP argument, a wire message or a log keeps its plain
// string. A kind the subpackages share is in internal/browser/ids.

// DownloadKey names one download in its thread's list (ThreadDownload.ID),
// which the pane's download actions pass back. The Manager numbers it as the
// download is admitted; it is not the engine's id (downloadID).
type DownloadKey string

// frameID is an engine's frame identity, which a download start and a
// request's owners name: a CDP frame id, whose main frame's is its page's
// target id; a WebKit or fake page's handle, its only frame; or, on the
// hosted engine, the launcher page id a download report names.
type frameID string

// jarKey identifies a cookie jar (jarIdentity.key): the Manager's key for
// its open scope and the identity an engine derives its profile from
// (profileOptions.Jar). A worktree jar's key is its canonical workspace.
type jarKey string

// profileHandle names one engine profile (engineProfile.Handle) in the
// engine's reports: a popup's and a popup download's profile. An engine
// derives it from its jar's key or numbers it; the hosted engine's is the
// WebView2 profile name the launcher's directives carry.
type profileHandle string

// downloadID is an engine's id for one download, unique among the engine's
// running downloads: a headless Chromium download GUID, a WebKit engine's
// download number, or the launcher's download id on the hosted engine. A
// download log entry carries it as DownloadInfo.ID.
type downloadID string
