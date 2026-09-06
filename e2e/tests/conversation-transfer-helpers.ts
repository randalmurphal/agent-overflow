// The real Codex app-server resolves a durable transcript through thread/read.
// The mock has no native index: supply that one response for an inert fixture.
export function codexTransferScenario(id?: string, path?: string): object {
  return {
    version: 1, name: 'codex-conversation-transfer', provider: 'codex',
    onStart: [{ delayMs: 1 }], turns: [], afterTurns: 'exit',
    codex: { responses: id && path ? {
      'thread/read': JSON.stringify({
        jsonrpc: '2.0', id: '${REQUEST_ID}',
        result: { thread: { id, path, status: { type: 'idle', activeFlags: [] } } },
      }).replace('"${REQUEST_ID}"', '${REQUEST_ID}'),
    } : {} },
  };
}
