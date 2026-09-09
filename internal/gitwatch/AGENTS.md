# Git status watcher

This package shares one watcher per canonical workspace, computes an initial
`git.GitStatus`, and publishes refreshed statuses to subscribers after
filesystem changes, explicit refresh requests, and liveness polling.

`NewManager` requires `StatusFn`. `FastStatusFn` is the network-free initial
and liveness path; `StatusFn` performs full refreshes. Multiple subscribers to
one workspace share cached status and watch roots.

Watch roots include the workspace, pruned content subtrees, linked-worktree Git
metadata, and configured ignore files. Canonicalize them, reject system roots,
deduplicate without dropping trigger-bearing roots, and never recursively
follow repository symlink trees. Root changes and ignore/config/index changes
must rebuild the installed watch set.

Filesystem delivery is an invalidation signal. Coalesce bursts, recompute
status, and use bounded subscriber channels so a slow client cannot block the
watch loop. If watcher installation or reinstallation fails, keep status fresh
through the polling fallback. Liveness probes use the fast status function and
restore filesystem watching when possible.

`RequestRefresh` is a no-op when nobody watches the workspace, but logs a path
that cannot be canonicalized. Subscription and manager close are idempotent.
The final subscriber stops its watcher; `Manager.Close` waits for all watcher
goroutines and closes update channels before returning.

Tests cover shared subscriptions, index and ref changes, linked worktrees,
ignored-tree rebuilds, dropped watchpoints, polling recovery, missed-event
detection, slow subscribers, liveness, and shutdown races.
