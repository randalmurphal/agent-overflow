# Conversation transfers

One global dialog serves the thread menu and the composer computer picker.
Move retains the conversation identity; Copy/fork creates an independent native
session and leaves the original usable. Keep those choices explicit.

The controller in `stores/conversationTransfers.svelte.ts` captures both computers
and one operation ID before its first await. A lost reply retries that same
request. Recovery reads the public intent and accepted destination project;
never store an offer grant in UI state, localStorage, logs or error messages.
Only the computers own transfer jobs, archives and activation proof.

The form locks accepted coordinates. A nested Add Project browser stays on the
destination already selected by the transfer. Offline computers stay visible.
Completed transfers can be followed by another copy of the same original.

Computer status lists remain bounded to the server's 100 rows and mount their
contents only when expanded. Events update one computer's signal; reconnect
reads merge intervening events instead of losing other recovered operations.
Unknown capability versions never attempt transfer RPCs. Cancellation controls
reflect the host protocol: an unprepared recipient can discard setup, while a
prepared recipient requires source cancellation and a committed move can only
finish. Errors stay visible beside the affected operation.
