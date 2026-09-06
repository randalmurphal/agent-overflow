import { isNativeShell } from './platform';
import { unthenable } from './plugins';

export interface SocketEvent {
  id: string;
  type: 'open' | 'message' | 'close' | 'error';
  data: string;
  code: number;
}

export interface NetworkPlugin {
  getCapabilities?(): Promise<{ computerRoutes?: boolean }>;
  httpStart(options: { id: string; url: string; pin: string; method: string; headers: Record<string, string>; length: number }): Promise<void>;
  httpHeaders(options: { id: string }): Promise<{ status: number; headers: Record<string, string> }>;
  httpWrite(options: { id: string; data: string; end: boolean }): Promise<void>;
  httpRead(options: { id: string }): Promise<{ data: string }>;
  httpClose(options: { id: string }): Promise<void>;
  socketOpen(options: { id: string; url: string; pin: string }): Promise<void>;
  socketSend(options: { id: string; data: string }): Promise<void>;
  socketAck(options: { id: string }): Promise<void>;
  socketClose(options: { id: string }): Promise<void>;
  addListener(event: 'socket', handler: (event: SocketEvent) => void): Promise<{ remove(): Promise<void> }>;
}

let loaded: Promise<NetworkPlugin> | undefined;
export function networkPlugin(): Promise<NetworkPlugin> {
  return loaded ??= load().catch((error) => { loaded = undefined; throw error; });
}

async function load(): Promise<NetworkPlugin> {
  if (!isNativeShell()) throw new Error('Private LAN certificates require the installed app.');
  const { registerPlugin, Capacitor } = await import('@capacitor/core');
  if (!Capacitor.isPluginAvailable('Network')) {
    throw new Error('Install the latest Android APK to connect over LAN. Your current APK can still connect through Tailscale.');
  }
  return unthenable(registerPlugin<NetworkPlugin>('Network'));
}
