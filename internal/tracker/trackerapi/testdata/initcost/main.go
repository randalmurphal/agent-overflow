// Command initcost links the trackerapi package and nothing else of this
// module, so GODEBUG=inittrace=1 reports the package's own init cost,
// free of test variables (TestInitCost).
package main

import _ "agent-overflow/internal/tracker/trackerapi"

func main() {}
