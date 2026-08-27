package browser

import "context"

const ServerName = "agent-overflow-browser"

type Config struct {
	Enabled               bool
	ShowWindow            bool
	PersistSiteData       bool
	AllowOutsideWorkspace bool
}

type Access struct {
	ThreadID    string
	Workspace   string
	ProjectRoot string
}

type PageInfo struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Title string `json:"title"`
}

type Snapshot struct {
	PageInfo
	Text     string            `json:"text"`
	Elements []SnapshotElement `json:"elements"`
}

type SnapshotElement struct {
	Selector    string `json:"selector"`
	Tag         string `json:"tag"`
	Role        string `json:"role,omitempty"`
	Text        string `json:"text,omitempty"`
	Label       string `json:"label,omitempty"`
	Type        string `json:"type,omitempty"`
	Href        string `json:"href,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
}

type OpenOptions struct {
	PageID  string
	NewPage bool
}

type TypeOptions struct {
	PageID   string
	Selector string
	Text     string
	Clear    bool
}

type ScreenshotOptions struct {
	PageID   string
	FullPage bool
}

type Controller interface {
	Open(context.Context, Access, string, OpenOptions) (PageInfo, error)
	OpenFile(context.Context, Access, string, OpenOptions) (PageInfo, error)
	Pages(context.Context, Access) ([]PageInfo, error)
	ClosePage(context.Context, Access, string) error
	Snapshot(context.Context, Access, string) (Snapshot, error)
	Screenshot(context.Context, Access, ScreenshotOptions) ([]byte, error)
	Click(context.Context, Access, string, string) (PageInfo, error)
	Type(context.Context, Access, TypeOptions) (PageInfo, error)
	Press(context.Context, Access, string, string) (PageInfo, error)
	Scroll(context.Context, Access, string, string, float64, float64) (PageInfo, error)
	Wait(context.Context, Access, string, string, int) (PageInfo, error)
	History(context.Context, Access, string, string) (PageInfo, error)
	Evaluate(context.Context, Access, string, string) (any, error)
	CloseThread(context.Context, string) error
	Close() error
	ClearSiteData(context.Context) error
	Reconfigure(Config) error
}
