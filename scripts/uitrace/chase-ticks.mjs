#!/usr/bin/env node
// Replay the scroll spring's per-tick records from a ui-trace bookmark.
//   node scripts/uitrace/chase-ticks.mjs <bookmark.jsonl> [--before 30] [--all]
// Prints each chase that ended in the window before the bug marker: its cadence
// totals, then the tick sequence run-length encoded as step/frames pairs so
// holes (frames > 1), uneven pixel rhythm and target arrivals are readable.
import { createReadStream } from 'node:fs';
import { createInterface } from 'node:readline';

const args = process.argv.slice(2);
const file = args.find((a) => !a.startsWith('--'));
if (!file) {
  console.error('usage: chase-ticks.mjs <bookmark.jsonl> [--before <seconds>] [--all]');
  process.exit(2);
}
const beforeIndex = args.indexOf('--before');
const beforeMs = (beforeIndex >= 0 ? Number(args[beforeIndex + 1]) : 30) * 1000;
const all = args.includes('--all');

const markers = [];
const chases = [];
const chunks = new Map();
const rl = createInterface({ input: createReadStream(file), crlfDelay: Infinity });
for await (const line of rl) {
  let record;
  try { record = JSON.parse(line); } catch { continue; }
  if (record.label === 'user.bugReport') markers.push(record);
  else if (record.label === 'scroll.spring.chase') chases.push(record);
  else if (record.label === 'scroll.spring.ticks') {
    const key = record.data.chaseId;
    if (!chunks.has(key)) chunks.set(key, []);
    chunks.get(key).push(record.data);
  }
}
if (markers.length === 0 && !all) {
  console.error('no user.bugReport marker; pass --all to list every chase');
  process.exit(1);
}

function rle(pairs) {
  const out = [];
  let previous = null;
  let count = 0;
  for (const pair of pairs) {
    if (pair === previous) count += 1;
    else {
      if (previous !== null) out.push(count > 1 ? `${previous}x${count}` : previous);
      previous = pair;
      count = 1;
    }
  }
  if (previous !== null) out.push(count > 1 ? `${previous}x${count}` : previous);
  return out.join(' ');
}

function describe(chase, marker) {
  const d = chase.data;
  const n = (value) => (value === undefined ? 'n/a' : value);
  const offset = marker ? ((chase.at - marker.at) / 1000).toFixed(2) + 's' : `at ${chase.at}`;
  console.log(`\n== chase ended ${offset}: ${d.durationMs}ms, ${d.ticks} ticks, ${d.writeTicks} writes, period ${n(d.periodMs)}ms`);
  console.log(`   dropped frames ${n(d.droppedFrames)} (worst hole ${n(d.maxHoleFrames)}), late ticks ${n(d.lateTicks)}, uneven writes ${n(d.unevenWrites)}, step jumps ${n(d.stepJumps)}, fallback ticks ${n(d.fallbackTicks)}, target changes ${d.targetChanges}`);
  const parts = (chunks.get(d.chaseId) ?? []).sort((a, b) => a.chunk - b.chunk);
  if (parts.length === 0) {
    console.log('   (no per-tick records; build predates them or the chase was under 3 ticks)');
    return;
  }
  const period = parts[0].periodMs || d.periodMs || 0;
  const pairs = [];
  let lateSum = 0;
  let lateMax = 0;
  for (const part of parts) {
    for (let i = 0; i < part.step.length; i++) {
      const frameMs = part.frame[i] / 10;
      const frames = period > 0 && frameMs > 0 ? Math.max(1, Math.round(frameMs / period)) : 1;
      const flags = part.flags[i];
      const tag = (flags & 1 ? 'T' : '') + (flags & 4 ? 'F' : '') + (flags & 8 ? 'P' : '');
      pairs.push(`${part.step[i]}${frames > 1 ? '/' + frames : ''}${tag}`);
      lateSum += part.late[i] / 10;
      if (part.late[i] / 10 > lateMax) lateMax = part.late[i] / 10;
    }
  }
  console.log(`   callback lateness mean ${(lateSum / pairs.length).toFixed(2)}ms max ${lateMax.toFixed(1)}ms`);
  console.log('   ticks (step[/frames][T target changed][F fallback][P parked]):');
  console.log('   ' + rle(pairs));
}

if (all) {
  for (const chase of chases) describe(chase, null);
} else {
  for (const marker of markers) {
    console.log(`\n#### marker seq ${marker.seq} at ${marker.at}`);
    for (const chase of chases) {
      if (chase.at <= marker.at && chase.at >= marker.at - beforeMs) describe(chase, marker);
    }
  }
}
