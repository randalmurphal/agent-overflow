// The channels the backend narrows per watched entity — the TS mirror of
// internal/transport/event_channels.go's `EntityFiltered` column, pinned to
// it by TestFrontendEntityFilteredChannelsMatch in that package.
//
// The client needs the list for exactly ONE reason, and it is not
// rendering: a frame the server withholds still consumed its channel's
// sequence number, so the next frame that DOES arrive on that channel skips
// forward. `wsClient.handleEventEntry`'s forward-skip heuristic reads a skip
// as "the server's fanout dropped events" and answers with a full resync
// (`eventsTransportGap.ts`), which on a narrowed channel would fire for
// every frame addressed to a thread this client is not watching. So the
// heuristic is exempted for exactly these channels, and only while a watch
// filter is actually armed on the connection.
//
// The exemption is narrow on purpose. Explicit `gap:true` markers keep
// working on these channels unchanged — those are a server statement about
// a real loss, honoured before the heuristic ever runs — and every other
// channel keeps the heuristic in full.
//
// A name here that the backend does not filter is harmless (an exemption
// that never applies); a name the backend filters and this list omits is
// the resync storm. The drift guard fails in both directions rather than
// trusting that asymmetry.
export const ENTITY_FILTERED_CHANNELS: readonly string[] = [
  'highlight:diff_seed',
  'highlight:seed',
  'provider:item_event',
];

const entityFilteredSet = new Set(ENTITY_FILTERED_CHANNELS);

/** Whether a watch filter narrows this channel's frames server-side. */
export function isEntityFilteredChannel(channel: string): boolean {
  return entityFilteredSet.has(channel);
}
