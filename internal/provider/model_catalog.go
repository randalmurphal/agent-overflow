package provider

// CatalogProvenance names where a model list came from, so a client can tell a
// shipped fallback apart from an answer the installed binary gave. Without it
// a cold start's un-enriched list is indistinguishable from a probe that
// genuinely no longer lists a model, and the only safe reading of absence —
// "nobody has asked the binary yet" — is unavailable.
type CatalogProvenance string

const (
	// CatalogShipped — AO's hand-maintained list. Not authoritative for the
	// account: the binary has not answered for this identity yet.
	CatalogShipped CatalogProvenance = "shipped"
	// CatalogProbed — the shipped Claude list enriched by what a zero-token
	// probe of this binary reported, live or restored from the account's
	// persisted record.
	CatalogProbed CatalogProvenance = "probed"
	// CatalogLive — the Codex app-server `model/list` answer, which replaces
	// the shipped list outright.
	CatalogLive CatalogProvenance = "live"
)

// ModelCatalog is one provider's model list plus where it came from.
type ModelCatalog struct {
	Models     []ModelInfo       `json:"models"`
	Provenance CatalogProvenance `json:"provenance"`
}
