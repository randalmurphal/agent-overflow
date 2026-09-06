import { certificatePin } from '../native/networkTrust';
import { pinnedFetch } from '../native/networkHttp';

/** App HTTP uses ordinary fetch except for a native, explicitly paired
 * private certificate. This leaves external links and browser policy intact. */
export const networkFetch: typeof fetch = async (input, init) => {
  const url = input instanceof Request ? input.url : String(input);
  const pin = certificatePin(url);
  if (!pin) return globalThis.fetch(input, init);
  return pinnedFetch(input, init, pin);
};
