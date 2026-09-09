package provider

// UsageProgress is an absolute snapshot of reported tokens in one accounting
// segment. A provider starts a new segment after each result, including an
// interrupted result with no accounting. Scope survives those boundaries but
// changes with the provider process. Final ModelUsage deltas consume pending
// tokens across segments in that scope. Progress never supplies invented cost.
type UsageProgress struct {
	Scope      string
	Segment    string
	ModelUsage []ModelTokenUsage
}
