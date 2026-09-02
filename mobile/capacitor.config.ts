import type { CapacitorConfig } from '@capacitor/cli';

// The shell's whole configuration, and every line of it is load-bearing.
//
// `webDir` is `../frontend/dist`: the SAME bundle the desktop ships,
// unforked. There is no mobile SPA — the compact layout is a layout mode
// of the one app, chosen from the viewport
// (`frontend/src/lib/stores/layoutMode.svelte.ts`), which is also what
// makes Playwright's compact project a real test of this shell.
//
// `server.hostname` + `androidScheme: 'https'` fix the page origin at
// `https://shell.agent-overflow.invalid`, which the Go side admits as the
// constant `transport.ShellOrigin`. Change either half here and that
// constant stops matching, so the two name each other. `.invalid` is
// reserved (RFC 6761 §6.4) and can never resolve, which is why one exact
// origin is a narrower door than any pattern: no page on any network can
// hold it, and the WebView has it only because Capacitor assigns its
// document that authority locally.
//
// `CapacitorHttp` is OFF. It would intercept `fetch` and route it through
// the native HTTP stack, which is the opposite of what this app needs:
// the transport is a WebSocket the WebView opens itself over the
// tailnet's WebPKI TLS, and the fetches beside it carry a session header
// and a device proof that the interceptor has no reason to understand.
// The WebView's own `fetch` and `WebSocket` are the whole transport.
const config: CapacitorConfig = {
  appId: 'dev.agentoverflow.app',
  appName: 'Agent Overflow',
  webDir: '../frontend/dist',
  server: {
    androidScheme: 'https',
    hostname: 'shell.agent-overflow.invalid',
  },
  plugins: {
    CapacitorHttp: {
      enabled: false,
    },
  },
};

export default config;
