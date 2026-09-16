import type { BrowserContext, CDPSession, Page } from '@playwright/test';
import { createServer } from 'node:http';
import { connect, type Socket } from 'node:net';

/** Software ceremonies for cross-engine UI tests, not native WebAuthn evidence. */
export async function installSoftwarePasskeys(context: BrowserContext): Promise<void> {
  await context.addInitScript(() => {
    // Playwright's Linux WebKit supplies a static-method object where Safari
    // supplies the interface constructor. Complete only the test authenticator.
    if (isSecureContext && typeof window.PublicKeyCredential !== 'function') {
      const credential = Object.assign(class PublicKeyCredential {}, window.PublicKeyCredential);
      Object.defineProperty(window, 'PublicKeyCredential', { configurable: true, value: credential });
    }
  });
  await context.credentials.install();
}

/** Resolve one test HTTPS authority without engine-specific DNS flags or host edits. */
export async function passkeyDomainProxy(domain: string, port: number, targetHost: string) {
  const sockets = new Set<Socket>();
  const failures: Error[] = [];
  let offline = false;
  const server = createServer((_req, res) => { res.writeHead(403).end(); });
  const track = (socket: Socket, upstream = false) => {
    sockets.add(socket);
    socket.on('close', () => sockets.delete(socket));
    socket.on('error', (error: NodeJS.ErrnoException) => {
      // Browser cancellation closes an in-flight tunnel normally.
      if (upstream && error.code !== 'ECONNRESET' && error.code !== 'EPIPE') failures.push(error);
      socket.destroy();
    });
  };
  server.on('connection', socket => track(socket));
  server.on('clientError', (_error, socket) => {
    socket.end('HTTP/1.1 400 Bad Request\r\nConnection: close\r\n\r\n');
  });
  server.on('connect', (req, client, head) => {
    if (offline) {
      client.end('HTTP/1.1 503 Service Unavailable\r\nConnection: close\r\n\r\n');
      return;
    }
    if (req.url !== `${domain}:${port}`) {
      client.end('HTTP/1.1 403 Forbidden\r\n\r\n');
      return;
    }
    const upstream = connect(port, targetHost);
    track(upstream, true);
    upstream.once('connect', () => {
      client.write('HTTP/1.1 200 Connection Established\r\n\r\n');
      if (head.length) upstream.write(head);
      client.pipe(upstream).pipe(client);
    });
    client.once('close', () => upstream.destroy());
    upstream.once('close', () => client.destroy());
  });
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => { server.off('error', reject); resolve(); });
  });
  server.on('error', (error) => failures.push(error));
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('The test proxy has no TCP listener');
  return {
    server: `http://127.0.0.1:${address.port}`,
    setOffline(value: boolean) {
      offline = value;
      if (offline) for (const socket of sockets) socket.destroy();
    },
    async close() {
      const closed = new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
      for (const socket of sockets) socket.destroy();
      await closed;
      if (failures.length) throw new AggregateError(failures, 'Passkey test proxy failed');
    },
  };
}

async function readCredentials(cdp: CDPSession, authenticatorId: string) {
  const { credentials } = await cdp.send('WebAuthn.getCredentials', { authenticatorId });
  return credentials;
}

export type VirtualCredential = Awaited<ReturnType<typeof readCredentials>>[number];
export interface Authenticator {
  credentials(): Promise<VirtualCredential[]>;
  adopt(credential: VirtualCredential): Promise<void>;
  setVerified(value: boolean): Promise<void>;
}

/** Install after navigation. Only the authenticator is virtual; verification is real. */
export async function attachAuthenticator(context: BrowserContext, page: Page): Promise<Authenticator> {
  const cdp = await context.newCDPSession(page);
  await cdp.send('WebAuthn.enable');
  const { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', {
    options: {
      protocol: 'ctap2',
      ctap2Version: 'ctap2_1',
      transport: 'internal',
      hasResidentKey: true,
      hasUserVerification: true,
      isUserVerified: true,
      automaticPresenceSimulation: true,
    },
  });
  return {
    credentials: () => readCredentials(cdp, authenticatorId),
    async adopt(credential) {
      await cdp.send('WebAuthn.addCredential', { authenticatorId, credential });
    },
    async setVerified(isUserVerified) {
      await cdp.send('WebAuthn.setUserVerified', { authenticatorId, isUserVerified });
    },
  };
}
