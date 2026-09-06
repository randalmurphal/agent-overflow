import { randomId } from '../utils/randomId';
import { networkPlugin, type NetworkPlugin, type SocketEvent } from './networkPlugin';

// One bridge listener for every connection. Native events carry a per-dial id,
// so an old socket's late close cannot affect its replacement.
const sockets = new Map<string, PinnedSocket>();
let listener: Promise<NetworkPlugin> | undefined;

function listen(): Promise<NetworkPlugin> {
  return listener ??= networkPlugin().then(async (plugin) => {
    await plugin.addListener('socket', (event) => sockets.get(event.id)?.receive(event));
    return plugin;
  }).catch((error) => { listener = undefined; throw error; });
}

export class PinnedSocket {
  private readonly events = new EventTarget();
  readyState = 0;
  private readonly id = randomId();
  private plugin: NetworkPlugin | undefined;
  private started = false;

  constructor(url: string, pin: string) {
    sockets.set(this.id, this);
    void this.connect(url, pin);
  }

  private async connect(url: string, pin: string): Promise<void> {
    try {
      const plugin = await listen();
      if (this.readyState !== 0) return;
      this.plugin = plugin;
      this.started = true;
      await plugin.socketOpen({ id: this.id, url, pin });
      // Closing during the asynchronous native open must close what it just
      // created, even if the earlier close arrived before it existed.
      if (sockets.get(this.id) !== this) await plugin.socketClose({ id: this.id });
    } catch (error) { this.fail(error instanceof Error ? error.message : String(error)); }
  }

  addEventListener<K extends keyof WebSocketEventMap>(type: K, listener: (event: WebSocketEventMap[K]) => void): void {
    this.events.addEventListener(type, listener as EventListener);
  }

  send(data: string): void {
    if (this.readyState !== 1 || !this.plugin) throw new Error('Connection is not open');
    void this.plugin.socketSend({ id: this.id, data }).catch((error) => this.fail(String(error)));
  }

  close(code = 1000, reason = ''): void {
    if (this.readyState === 3) return;
    if (this.started) void this.plugin?.socketClose({ id: this.id }).catch(() => undefined);
    this.finish(code, reason);
  }

  receive(event: SocketEvent): void {
    if (this.readyState === 3) return;
    if (event.type === 'open') {
      if (this.readyState !== 0) return;
      this.readyState = 1;
      this.events.dispatchEvent(new Event('open'));
    } else if (event.type === 'message' && this.readyState === 1) {
      this.events.dispatchEvent(new MessageEvent('message', { data: event.data }));
      void this.plugin?.socketAck({ id: this.id }).catch((error) => this.fail(String(error)));
    } else if (event.type === 'error') this.fail(event.data);
    else if (event.type === 'close') this.finish(event.code, event.data);
  }

  private fail(message: string): void {
    if (this.readyState === 3) return;
    this.events.dispatchEvent(new ErrorEvent('error', { message }));
    this.close(1006, message);
  }

  private finish(code: number, reason: string): void {
    this.readyState = 3;
    sockets.delete(this.id);
    this.events.dispatchEvent(new CloseEvent('close', { code, reason, wasClean: code === 1000 }));
  }
}
