import { randomId } from '../utils/randomId';
import { base64ToBytes } from '../utils/base64';
import { networkPlugin, type NetworkPlugin } from './networkPlugin';

const CHUNK_BYTES = 64 * 1024;

/** A fetch-shaped adapter for paired private TLS. Backpressure crosses the
 * native boundary one chunk at a time in both directions. Never patch fetch. */
export async function pinnedFetch(input: RequestInfo | URL, init: RequestInit | undefined, pin: string): Promise<Response> {
  const plugin = await networkPlugin();
  const request = new Request(input, init);
  request.signal.throwIfAborted();
  // All current bodies are File/Blob or small JSON. Blob streams preserve
  // file-backed storage; serial chunks avoid a full-file base64 allocation.
  const body = init?.body instanceof Blob ? init.body : request.body ? await request.blob() : null;
  const id = randomId();
  let closed = false;
  const close = async (): Promise<void> => {
    if (closed) return;
    closed = true;
    request.signal.removeEventListener('abort', aborted);
    await plugin.httpClose({ id }).catch(() => undefined);
  };
  const aborted = (): void => { void close(); };
  try {
    await plugin.httpStart({ id, url: request.url, pin, method: request.method,
      headers: Object.fromEntries(request.headers), length: body?.size ?? -1 });
    request.signal.addEventListener('abort', aborted, { once: true });
    request.signal.throwIfAborted();
    if (body) await writeBody(plugin, id, body, request.signal);
    const { status, headers } = await plugin.httpHeaders({ id });
    request.signal.throwIfAborted();
    // Native networking deliberately does not follow redirects: an auth POST
    // must never be repeated, nor may a ticket leave its paired computer.
    if (status >= 300 && status < 400) throw new Error('The computer redirected the connection. Check its address and pair again.');
    if (request.method === 'HEAD' || status === 204 || status === 205 || status === 304) {
      await close();
      return new Response(null, { status, headers });
    }
    const stream = new ReadableStream<Uint8Array<ArrayBuffer>>({
      async pull(controller) {
        try {
          request.signal.throwIfAborted();
          const { data } = await plugin.httpRead({ id });
          if (data) controller.enqueue(base64ToBytes(data));
          else { controller.close(); await close(); }
        } catch (error) { controller.error(error); await close(); }
      },
      cancel: close,
    });
    return new Response(stream, { status, headers });
  } catch (error) {
    await close();
    throw error;
  }
}

async function writeBody(plugin: NetworkPlugin, id: string, body: Blob, signal: AbortSignal): Promise<void> {
  for (let at = 0; at < body.size; at += CHUNK_BYTES) {
    signal.throwIfAborted();
    const chunk = new Uint8Array(await body.slice(at, at + CHUNK_BYTES).arrayBuffer());
    let binary = '';
    for (let i = 0; i < chunk.length; i += 8192) binary += String.fromCharCode(...chunk.subarray(i, i + 8192));
    await plugin.httpWrite({ id, data: btoa(binary), end: at + chunk.length === body.size });
  }
  if (body.size === 0) await plugin.httpWrite({ id, data: '', end: true });
}
