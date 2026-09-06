import { fileURLToPath } from 'node:url';
import { spawnContained } from '../src/harness-process.js';
import type { HarnessApp } from '../src/harness.js';

/** Run AO's CLI with the credential issued to an isolated mock-provider session. */
export async function runSessionCLI(harness: HarnessApp, threadID: string, args: string[]): Promise<{ code: number; stdout: string; stderr: string }> {
  const env = await harness.rpc<Record<string, string>>('HarnessSessionEnv', threadID);
  if (!env.AO_ENDPOINT || !env.AO_TOKEN) throw new Error('The mock provider session has no scoped CLI credential');
  const binary = process.env.AO_HARNESS_BIN ?? fileURLToPath(new URL('../../bin/agent-overflow', import.meta.url));
  return new Promise((resolve, reject) => {
    const child = spawnContained(binary, args, {
      env: { PATH: process.env.PATH ?? '', HOME: harness.bootstrap.dataDir, ...env },
      stdio: ['ignore', 'pipe', 'pipe'], memoryLimitBytes: 512 * 1024 * 1024,
    });
    let stdout = '', stderr = '';
    let failure: Error | undefined;
    const timeout = setTimeout(() => { failure = new Error('Session CLI did not finish in 20 seconds'); child.kill('SIGKILL'); }, 20_000);
    const append = (value: string, chunk: Buffer): string => {
      if (value.length + chunk.length > 1024 * 1024) {
        failure = new Error('Session CLI exceeded the test output limit');
        child.kill('SIGKILL');
        return value;
      }
      return value + chunk.toString();
    };
    child.stdout!.on('data', (chunk: Buffer) => { stdout = append(stdout, chunk); });
    child.stderr!.on('data', (chunk: Buffer) => { stderr = append(stderr, chunk); });
    child.once('error', (error) => { clearTimeout(timeout); reject(error); });
    child.once('close', (code) => {
      clearTimeout(timeout);
      if (failure) reject(failure);
      else resolve({ code: code ?? -1, stdout, stderr });
    });
  });
}
