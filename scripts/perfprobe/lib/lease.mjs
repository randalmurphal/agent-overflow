import { closeSync, mkdirSync, openSync, readFileSync, readdirSync, unlinkSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { randomUUID } from 'node:crypto';
import { instrumentsCompatible } from './policy.mjs';

const localLeases = new Map();

function processIsAlive(pid) {
  if (!Number.isInteger(pid) || pid <= 0) return false;
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    if (error?.code === 'EPERM') return true;
    return false;
  }
}

function leaseRoot(manifest) {
  const configured = manifest.leasePath || manifest.lease?.path || manifest.lease?.directory
    || process.env.AO_PERFPROBE_LEASE;
  if (!configured) throw new Error('perfprobe: instance manifest has no shared probe lease path');
  return path.resolve(configured);
}

function readHolder(file) {
  try {
    return JSON.parse(readFileSync(file, 'utf8'));
  } catch (error) {
    throw new Error(`perfprobe: invalid probe lease ${file}: ${error.message}`);
  }
}

function reapStale(root) {
  for (const name of readdirSync(root, { withFileTypes: true })) {
    if (!name.isFile() || !name.name.endsWith('.json')) continue;
    const file = path.join(root, name.name);
    const holder = readHolder(file);
    if (!processIsAlive(Number(holder.pid))) unlinkSync(file);
  }
}

/** Acquire the process-wide lease for this probe's instrument kind. */
export function acquireProbeLease(manifest, probe, kind) {
  const root = leaseRoot(manifest);
  const localKey = `${root}\0${kind}`;
  const existing = localLeases.get(localKey);
  if (existing) {
    existing.refs += 1;
    return () => releaseProbeLease(existing, localKey);
  }

  mkdirSync(root, { recursive: true, mode: 0o700 });
  reapStale(root);
  const token = `${process.pid}-${randomUUID()}`;
  const file = path.join(root, `${token}.json`);
  const holder = {
    version: 1,
    pid: process.pid,
    probe,
    kind,
    startedAt: new Date().toISOString(),
  };
  // A holder is created atomically. Recheck every current holder after the
  // create. If a competing process won the same instant, remove ours and
  // refuse. This is conservative and cannot turn an incompatible pairing
  // into a shared measurement.
  const fd = openSync(file, 'wx', 0o600);
  try {
    writeFileSync(fd, JSON.stringify(holder));
  } finally {
    closeSync(fd);
  }
  try {
    for (const name of readdirSync(root)) {
      if (!name.endsWith('.json') || name === path.basename(file)) continue;
      const other = readHolder(path.join(root, name));
      if (!instrumentsCompatible(kind, String(other.kind))) {
        throw new Error(
          `perfprobe: instrument ${kind} is incompatible with active ${other.kind} probe ${other.probe} (pid ${other.pid})`,
        );
      }
    }
  } catch (error) {
    unlinkSync(file);
    throw error;
  }
  const state = { root, file, kind, refs: 1 };
  localLeases.set(localKey, state);
  return () => releaseProbeLease(state, localKey);
}

function releaseProbeLease(state, localKey) {
  state.refs -= 1;
  if (state.refs > 0) return;
  localLeases.delete(localKey || `${state.root}\0${state.kind}`);
  try {
    unlinkSync(state.file);
  } catch (error) {
    if (error?.code !== 'ENOENT') console.warn(`perfprobe: failed to release probe lease: ${error.message}`);
  }
}

export function activeProbeLeases() {
  return [...localLeases.values()].map(({ root, file, refs }) => ({ root, file, refs }));
}
