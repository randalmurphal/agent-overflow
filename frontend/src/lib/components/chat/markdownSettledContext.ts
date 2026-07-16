// Svelte context key for the warm-gate "ChatMarkdown has signaled
// settled-once since arm" aggregator. MessageTimeline owns the
// aggregation boolean and exposes a single mark function via
// setContext() on every armWarmup() cycle. ChatMarkdown reads it via
// getContext() and calls it once Streamdown's `onsettled` fires.
// Non-timeline ChatMarkdowns get `undefined` and skip the signal.
//
// Exported as a string constant (not a Symbol) so test code can also
// register a fake aggregator without importing from this module —
// matches how other test fixtures stub context-based bindings.
export const CHAT_MARKDOWN_SETTLED_CONTEXT = 'agent-overflow:chat-markdown-settled';

// Companion context: live mounted-ChatMarkdown presence. MessageTimeline
// registers a function that increments a live count and returns the
// matching decrement; ChatMarkdown calls it from a $effect so the effect
// cleanup unregisters on unmount. The count feeds the warm-gate
// quietContextSignal: a mounted window containing NO markdown has no
// async typesetting to wait for, so the signal reports settled by
// absence instead of holding the gate to the 2.5s failsafe (threads
// whose visible tail is all tool output / terminals / images).
export const CHAT_MARKDOWN_PRESENCE_CONTEXT = 'agent-overflow:chat-markdown-presence';
