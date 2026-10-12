// Every product binary the suite runs (bin/agent-overflow and the bin/ao-*
// tools `make harness-build` writes beside it) starts through this module,
// so its environment is always launchEnvironment's allowlist plus what the
// caller passes. tests/product-launch.spec.ts fails when any other file in
// src/ or tests/ can start a process and names a product binary.
import { execFile as execFileCallback, type ChildProcess, type ExecFileOptions, type SpawnOptions } from 'node:child_process';
import * as path from 'node:path';
import { promisify } from 'node:util';
import { launchEnvironment } from './harness-env.ts';
import { spawnContained } from './harness-process.ts';

const execFile = promisify(execFileCallback);

/**
 * A binary of this build: `agent-overflow` is the backend under test,
 * $AO_HARNESS_BIN, else <repo>/bin/agent-overflow; an `ao-*` tool is the one
 * `make harness-build` wrote to <repo>/bin, wherever the backend is (the
 * release workflow points AO_HARNESS_BIN at the release's own binary).
 */
export function productBinary(name: 'agent-overflow' | `ao-${string}`): string {
  const bin = path.join(path.resolve(import.meta.dirname, '..', '..'), 'bin');
  if (name === 'agent-overflow') return process.env.AO_HARNESS_BIN ?? path.join(bin, name);
  return path.join(bin, name);
}

/** Start a product binary under spawnContained with the launch environment plus `env`. */
export function spawnProduct(
  binary: string,
  args: string[],
  options: Omit<SpawnOptions, 'env'> & { memoryLimitBytes: number; env?: Record<string, string> },
): ChildProcess {
  const { env, ...spawnOptions } = options;
  return spawnContained(binary, args, { ...spawnOptions, env: launchEnvironment(env) });
}

/** Run a product binary to completion with the launch environment plus `env`. */
export async function execProduct(
  binary: string,
  args: string[],
  options: Omit<ExecFileOptions, 'env' | 'encoding'> & { env?: Record<string, string> } = {},
): Promise<{ stdout: string; stderr: string }> {
  const { env, ...execOptions } = options;
  return await execFile(binary, args, { ...execOptions, env: launchEnvironment(env), encoding: 'utf8' });
}
