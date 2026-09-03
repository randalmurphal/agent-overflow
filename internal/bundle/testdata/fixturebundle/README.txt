One fixture dist tree, shared by two implementations of the same rule.

internal/bundle (Go) serves the manifest; frontend/scripts/bundleId.ts
(the Vite plugin) stamps the id into a built bundle. If those two ever
disagree, a shell would download a bundle it is already running, forever.
So both hash THIS directory in their own test and both compare the answer
against fixturebundle.id one level up.

It deliberately contains the two exclusions the rule states: a source map
and a bundle-id.txt, neither of which may reach the manifest.
