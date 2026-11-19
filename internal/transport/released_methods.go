package transport

// ReleasedMethods exists for windows on the previous release: it answers,
// under their released names and ids, the retired methods such a window
// calls from a screen this release still has, each with the empty value it
// reads as "none" (ListDiscussionsForThread runs whenever its model menu
// opens). The retired features' own screens stay unsupported.
type ReleasedMethods struct{}

func (*ReleasedMethods) ListDiscussionsForThread(string) []struct{} { return []struct{}{} }

var releasedMethods = []MethodMeta{
	{Name: "ListDiscussionsForThread", ID: 2502562885, Scope: "threads:read", Route: "thread"},
}
