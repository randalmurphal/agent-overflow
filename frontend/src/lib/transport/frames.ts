// Wire-frame shapes for the HTTP+WS transport — the TypeScript mirror
// of /internal/transport/frame.go ServerFrame / ClientFrame. We allow
// `unknown` for `result` and `data` because the generated bindings
// unpack them via Create.* factories on the caller's side, not here.

export interface ServerRPCFrame {
  type: 'rpc';
  id: string;
  result?: unknown;
  error?: { code: string; message: string };
}

export interface ServerEventFrame {
  type: 'event';
  channel: string;
  seq: number;
  data: unknown;
  gap?: boolean;
}

export interface ServerBatchFrame {
  type: 'batch';
  events: Array<{
    channel: string;
    seq: number;
    data: unknown;
    gap?: boolean;
  }>;
}

export interface ServerReplayFrame {
  type: 'replay';
  id?: string;
}

// Server keepalive heartbeat (internal/transport/conn.go keepalive
// loop). Carries no payload; its arrival — like any frame's — refreshes
// the staleness watchdog, and seeing the first one marks the connection
// as heartbeat-backed so the watchdog can arm (see the wsClient's
// serverSendsHeartbeats).
export interface ServerPingFrame {
  type: 'ping';
}

// Server hello: the first frame on every connection
// (internal/transport/frame.go helloFrame). States what backend this is,
// what dialect it speaks, and what it can do, so a client seeds its
// compatibility state before any other frame lands.
//
// Nothing gates on `protocolVersion` — features negotiate through
// `capabilities`, which is why the version is typed as a plain number
// with no comparison helper anywhere. A backend too old to send this
// frame simply leaves the client with no hello, which reads as "no
// capabilities" and degrades rather than guessing.
export interface ServerHelloFrame {
  type: 'hello';
  protocolVersion: number;
  capabilities: string[];
  backendId?: string;
  serverTimeMs: number;
}

export type ServerFrame =
  | ServerRPCFrame
  | ServerEventFrame
  | ServerBatchFrame
  | ServerReplayFrame
  | ServerPingFrame
  | ServerHelloFrame;

export interface ClientRPCFrame {
  type: 'rpc';
  id: string;
  methodId?: number;
  method?: string;
  params: unknown[];
}

export interface ClientReplayFrame {
  type: 'replay';
  lastSeqByChannel: Record<string, number>;
}

export type ClientFrame = ClientRPCFrame | ClientReplayFrame;

// Logged strings (channel names, error messages) get clamped before
// reaching console / toast surfaces. Caps the worst-case noise from a
// pathological remote without losing the prefix that identifies the
// channel.
const LOG_STRING_MAX = 256;

// clampString truncates noisy / hostile log content before it reaches
// console or toast surfaces. The `…` suffix preserves the visual signal
// that the value was abbreviated.
export function clampString(value: string, max = LOG_STRING_MAX): string {
  if (value.length <= max) return value;
  return `${value.slice(0, max)}…`;
}

// extractRpcIdFromOversizedFrame pulls the `id` out of the leading
// portion of a frame too large to JSON.parse safely. ServerFrame's
// `id` is an RPC uuid emitted by call() and is always present at the
// frame's top level; a tolerant regex over the first ~256 chars is
// sufficient to recover it without parsing megabytes of payload.
// Returns null when the prefix doesn't contain an obvious id (frame
// is not an rpc response or shape changed).
const oversizedIdRegex = /"id"\s*:\s*"([^"]{1,128})"/;
export function extractRpcIdFromOversizedFrame(text: string): string | null {
  const prefix = text.slice(0, 256);
  const m = oversizedIdRegex.exec(prefix);
  return m ? m[1] : null;
}
