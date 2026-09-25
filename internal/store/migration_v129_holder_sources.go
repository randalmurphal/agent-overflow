package store

// holderSourcesV129SQL makes a holder's fork_source_thread_id name only the
// thread whose rows a split gave it. A split or a copy for a thread's
// readers reuses the holder they read right before the thread
// (reusableHolderTx), which must be one made for that thread's rows. A
// retired thread holds its own history, whose rows sit at positions its
// source's rows also take, so retireToHolderTx now clears the column;
// earlier builds kept the source there, and a holder's origin is not
// recorded anywhere else. Every existing holder loses it: the next split
// of its thread makes a new holder, which later splits reuse.
const holderSourcesV129SQL = `
UPDATE threads SET fork_source_thread_id = '' WHERE mode = 'holder';
`
