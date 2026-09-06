import { certificatePin } from '../native/networkTrust';
import { PinnedSocket } from '../native/networkSocket';
import { pairedComputerSocketRoute } from './deviceSession';
import { HOME_BACKEND, type BackendKey } from './backendKey';

export function createNetworkSocket(url: string, backend: BackendKey = HOME_BACKEND): WebSocket | PinnedSocket {
  const route = pairedComputerSocketRoute(backend, url);
  const pin = route ? route.pin : certificatePin(url);
  const target = route?.url ?? url;
  const socket = pin ? new PinnedSocket(target, pin) : new globalThis.WebSocket(target);
  if (route) {
    socket.addEventListener('error', route.failed);
    socket.addEventListener('close', (event) => { if (!event.wasClean) route.failed(); });
  }
  return socket;
}
