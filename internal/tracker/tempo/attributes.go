package tempo

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

// AttributeType is a work attribute's type as Tempo names it.
type AttributeType string

// The attribute types Tempo documents.
const (
	AttributeAccount  AttributeType = "ACCOUNT"
	AttributeCheckbox AttributeType = "CHECKBOX"
	AttributeText     AttributeType = "INPUT_FIELD"
	AttributeNumber   AttributeType = "INPUT_NUMERIC"
	AttributeList     AttributeType = "STATIC_LIST"
)

// Attribute is a work attribute a worklog can carry.
type Attribute struct {
	Key      string
	Name     string
	Type     AttributeType
	Required bool
	// Options are a static list's allowed values in Tempo's order; empty
	// for every other type. An account attribute allows the key of an
	// account Accounts lists (open ones, and closed and archived ones when
	// Settings.InactiveAccountsAllowed).
	Options []Option

	// optionsText lists the options for a problem message.
	optionsText string
}

// Option is one value a static list allows: Value is what a worklog
// stores and never changes; Name is what Tempo shows and can change.
type Option struct {
	Value string
	Name  string
}

type attributeJSON struct {
	Key      string            `json:"key"`
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	Required bool              `json:"required"`
	Values   []string          `json:"values"`
	Names    map[string]string `json:"names"`
}

// maxAttributes bounds the cached attribute list, options included.
const maxAttributes = 1000

// maxOptions bounds the options of all cached attributes together.
const maxOptions = 50_000

// Attributes lists the site's work attributes. The list is cached until
// DropCache or a write Tempo refuses; the caller gets its own copy.
func (c *Client) Attributes(ctx context.Context) ([]Attribute, error) {
	if err := c.admit(); err != nil {
		return nil, err
	}
	attrs, err := c.attributes(ctx)
	if err != nil {
		return nil, fmt.Errorf("tempo: %w", err)
	}
	out := make([]Attribute, len(attrs))
	for i, a := range attrs {
		out[i] = a
		out[i].Options = slices.Clone(a.Options)
	}
	return out, nil
}

// attributes is the cached list, shared: callers do not change it.
func (c *Client) attributes(ctx context.Context) ([]Attribute, error) {
	v, err := c.site.attributes.get(ctx, c.http.CredentialMark, func(ctx context.Context) ([]Attribute, trackerapi.CredentialMark, error) {
		var (
			out     []Attribute
			mark    trackerapi.CredentialMark
			first   = true
			options = 0
		)
		err := pages(ctx, c.http, trackerapi.Request{Path: "4/work-attributes"}, func(resp *trackerapi.Response, list []attributeJSON) error {
			// The list is the credential's when every page was drawn
			// under one read of it.
			if m := resp.CredentialMark(); first {
				mark, first = m, false
			} else if m != mark {
				mark = trackerapi.CredentialMark{}
			}
			for _, a := range list {
				options += len(a.Values)
				if len(out) >= maxAttributes || options > maxOptions {
					return fmt.Errorf("the site has more than %d work attributes or %d options, the most this client holds", maxAttributes, maxOptions)
				}
				attr := Attribute{Key: a.Key, Name: a.Name, Type: AttributeType(a.Type), Required: a.Required}
				for _, v := range a.Values {
					attr.Options = append(attr.Options, Option{Value: v, Name: a.Names[v]})
				}
				attr.optionsText = optionNames(attr.Options)
				out = append(out, attr)
			}
			return nil
		})
		return out, mark, err
	})
	if err != nil {
		return nil, fmt.Errorf("read work attributes: %w", err)
	}
	return v, nil
}

// Settings are the Tempo site settings a worklog's checks and a time
// grid use.
type Settings struct {
	// InactiveAccountsAllowed reports that a worklog may name a closed or
	// archived account (Tempo's "Users can log time for closed and
	// archived accounts", allowLoggingOnClosedAccount in the API).
	InactiveAccountsAllowed bool
	// WeekStart is the first day of Tempo's week.
	WeekStart time.Weekday
	// ApprovalPeriod is "WEEK" or "MONTH".
	ApprovalPeriod string
	// DescriptionRequired, RemainingEstimateRequired and DaysIntoFuture
	// are what Tempo's settings say about logging time. Whether the API
	// refuses a worklog against them is not documented, so the checks do
	// not apply them.
	DescriptionRequired       bool
	RemainingEstimateRequired bool
	DaysIntoFuture            int
}

type settingsJSON struct {
	AllowLoggingOnClosedAccount bool   `json:"allowLoggingOnClosedAccount"`
	ApprovalPeriod              string `json:"approvalPeriod"`
	WeekStart                   int    `json:"weekStart"`
	DescriptionOptional         *bool  `json:"worklogDescriptionOptional"`
	RemainingEstimateOptional   *bool  `json:"remainingEstimateOptional"`
	DaysIntoFuture              int    `json:"numberOfDaysAllowedIntoFuture"`
}

// Settings reads Tempo's global configuration, cached as Attributes is.
func (c *Client) Settings(ctx context.Context) (Settings, error) {
	if err := c.admit(); err != nil {
		return Settings{}, err
	}
	v, err := c.settings(ctx)
	if err != nil {
		return Settings{}, fmt.Errorf("tempo: %w", err)
	}
	return v, nil
}

func (c *Client) settings(ctx context.Context) (Settings, error) {
	v, err := c.site.settings.get(ctx, c.http.CredentialMark, func(ctx context.Context) (Settings, trackerapi.CredentialMark, error) {
		var raw settingsJSON
		resp, err := c.http.JSON(ctx, trackerapi.Request{Path: "4/globalconfiguration"}, &raw)
		if err != nil {
			return Settings{}, trackerapi.CredentialMark{}, err
		}
		s := Settings{
			InactiveAccountsAllowed:   raw.AllowLoggingOnClosedAccount,
			ApprovalPeriod:            raw.ApprovalPeriod,
			WeekStart:                 time.Monday,
			DescriptionRequired:       raw.DescriptionOptional != nil && !*raw.DescriptionOptional,
			RemainingEstimateRequired: raw.RemainingEstimateOptional != nil && !*raw.RemainingEstimateOptional,
			DaysIntoFuture:            raw.DaysIntoFuture,
		}
		// Tempo numbers days 1 (Monday) to 7 (Sunday).
		if raw.WeekStart >= 1 && raw.WeekStart <= 7 {
			s.WeekStart = time.Weekday(raw.WeekStart % 7)
		}
		return s, resp.CredentialMark(), nil
	})
	if err != nil {
		return Settings{}, fmt.Errorf("read settings: %w", err)
	}
	return v, nil
}

// siteCache holds what rarely changes on a site.
type siteCache struct {
	attributes cached[[]Attribute]
	settings   cached[Settings]
}

func (s *siteCache) drop() {
	s.attributes.drop()
	s.settings.drop()
}

// shut refuses new reads, joins the reads in flight and drops the values.
func (s *siteCache) shut() {
	s.attributes.shut()
	s.settings.shut()
}

// cached is one value read once and shared, while the transport holds
// the credential read its answer was drawn under: a token read again,
// forgotten or replaced after a refusal retires it, whichever call drew
// the answer that caused it (the token selects the Tempo tenant). An
// answer of no credential read is never kept. Concurrent callers share one
// read. The read runs on the first caller's context without its
// cancellation, so a caller that gives up leaves without ending a read
// others wait on; the transport's timeouts and Close still end it. A
// failed read is not kept. The reads run on goroutines the cache owns;
// Client.Close ends them through the transport and joins them.
type cached[T any] struct {
	mu       sync.Mutex
	value    T
	ok       bool
	mark     trackerapi.CredentialMark
	gen      uint64 // advanced by drop, so a read started before it is not kept
	inflight *loading[T]
	reads    sync.WaitGroup // the reads in flight, joined by shut
	closed   bool
}

// loading is one read in flight; v, err and kept are set before done
// closes.
type loading[T any] struct {
	done chan struct{}
	v    T
	err  error
	kept bool
}

// heldBy reports a value drawn under mark while the transport holds that
// read of the credential.
func heldBy(mark, now trackerapi.CredentialMark) bool {
	return mark == now && mark != (trackerapi.CredentialMark{})
}

func (c *cached[T]) get(ctx context.Context, now func() trackerapi.CredentialMark, load func(context.Context) (T, trackerapi.CredentialMark, error)) (T, error) {
	var zero T
	for {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		held := now()
		c.mu.Lock()
		if c.ok && heldBy(c.mark, held) {
			v := c.value
			c.mu.Unlock()
			return v, nil
		}
		if c.ok {
			// Drawn under a credential read the transport no longer holds.
			c.value, c.ok, c.mark = zero, false, trackerapi.CredentialMark{}
			c.gen++
		}
		if c.closed {
			c.mu.Unlock()
			return zero, &trackerapi.NotSentError{Err: ErrClosed}
		}
		l := c.inflight
		if l == nil {
			l = &loading[T]{done: make(chan struct{})}
			c.inflight = l
			gen := c.gen
			c.reads.Go(func() { c.run(context.WithoutCancel(ctx), gen, l, load) })
		}
		c.mu.Unlock()
		select {
		case <-l.done:
		case <-ctx.Done():
			return zero, ctx.Err()
		}
		switch {
		case l.err != nil:
			return zero, l.err
		case l.kept:
			return l.v, nil
		}
		// Dropped while it was read: read again.
	}
}

func (c *cached[T]) run(ctx context.Context, gen uint64, l *loading[T], load func(context.Context) (T, trackerapi.CredentialMark, error)) {
	v, mark, err := load(ctx)
	c.mu.Lock()
	l.v, l.err = v, err
	if c.inflight == l {
		c.inflight = nil
	}
	// A read not dropped meanwhile answers its callers; it is kept only
	// when drawn under one credential read.
	if err == nil && gen == c.gen {
		l.kept = true
		if mark != (trackerapi.CredentialMark{}) {
			c.value, c.ok, c.mark = v, true, mark
		}
	}
	c.mu.Unlock()
	close(l.done)
}

func (c *cached[T]) drop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero T
	c.value, c.ok, c.mark = zero, false, trackerapi.CredentialMark{}
	c.gen++
	c.inflight = nil
}

// shut refuses new reads, joins those in flight and drops the value.
func (c *cached[T]) shut() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.reads.Wait()
	c.drop()
}
