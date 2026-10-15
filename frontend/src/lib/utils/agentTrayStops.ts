// What an Agents Stop reports: one toast per Stop, every layer of its
// cascade folded in (the request, the spawned thread's turn, its
// Backgrounded items, its own agents), with what could not be stopped named
// rather than aborting the rest. Kept apart from the body so the wording is
// tested on its own.

import type { ThreadAgentStop } from '../stores/bindings';
import type { ToastType } from '../stores/toast.svelte';

export interface AgentStopReport {
  tone: ToastType;
  text: string;
  /** The first layer that failed, shown on the row until it settles. */
  firstFailure: string;
}

function plural(count: number, one: string, many: string): string {
  return `${count} ${count === 1 ? one : many}`;
}

function joinParts(parts: readonly string[]): string {
  if (parts.length <= 1) return parts.join('');
  return `${parts.slice(0, -1).join(', ')} and ${parts[parts.length - 1]}`;
}

function cascadeParts(interrupted: number, background: number, nested: number): string[] {
  const parts: string[] = [];
  if (interrupted > 0) parts.push(interrupted === 1 ? 'its turn' : plural(interrupted, 'turn', 'turns'));
  if (background > 0) parts.push(plural(background, 'background task', 'background tasks'));
  if (nested > 0) parts.push(plural(nested, 'agent of its own', 'agents of its own'));
  return parts;
}

/** The toast one row's Stop shows. */
export function agentStopReport(title: string, stop: ThreadAgentStop): AgentStopReport {
  const name = title || 'that agent';
  const failures = stop.failures ?? [];
  const firstFailure = failures[0] ?? '';
  if (stop.incoming) {
    if (stop.outcome === 'ended') return { tone: 'info', text: `Already delivered: ${name}.`, firstFailure };
    if (stop.outcome === 'failed') return { tone: 'error', text: `Could not withdraw ${name}: ${failures.join('; ')}`, firstFailure };
    const tail = failures.length > 0 ? ` Not done: ${failures.join('; ')}` : '';
    return { tone: failures.length > 0 ? 'warning' : 'success', text: `Withdrew ${name} before this thread's agent read it.${tail}`, firstFailure };
  }
  if (stop.outcome === 'failed') {
    return { tone: 'error', text: `Could not stop ${name}: ${failures.join('; ') || 'the stop failed'}`, firstFailure };
  }
  const parts = cascadeParts(stop.interrupted ? 1 : 0, stop.background ?? 0, stop.nested ?? 0);
  const head = stop.outcome === 'ended'
    ? parts.length > 0 ? `Already finished: ${name}; stopped ${joinParts(parts)}.` : `Already finished: ${name}.`
    : parts.length > 0 ? `Stopped ${name}: ${joinParts(parts)}.` : `Stopped ${name}.`;
  if (failures.length > 0) return { tone: 'warning', text: `${head} Not stopped: ${failures.join('; ')}`, firstFailure };
  return { tone: stop.outcome === 'ended' ? 'info' : 'success', text: head, firstFailure };
}

/** The one toast Stop All shows for every row it stopped. */
export function agentStopAllReport(stops: readonly ThreadAgentStop[]): { tone: ToastType; text: string } {
  if (stops.length === 0) return { tone: 'info', text: 'Nothing was running.' };
  let stopped = 0, ended = 0, failed = 0, withdrawn = 0, delivered = 0;
  let interrupted = 0, background = 0, nested = 0;
  const failures: string[] = [];
  for (const stop of stops) {
    failures.push(...(stop.failures ?? []));
    if (stop.incoming) {
      if (stop.outcome === 'stopped') withdrawn++;
      else if (stop.outcome === 'ended') delivered++;
      else failed++;
      continue;
    }
    if (stop.outcome === 'stopped') stopped++;
    else if (stop.outcome === 'ended') ended++;
    else failed++;
    if (stop.interrupted) interrupted++;
    background += stop.background ?? 0;
    nested += stop.nested ?? 0;
  }
  const sentences: string[] = [];
  if (stopped > 0) {
    const parts = cascadeParts(0, background, nested);
    if (interrupted > 0) parts.unshift(plural(interrupted, 'turn', 'turns'));
    sentences.push(`Stopped ${plural(stopped, 'agent', 'agents')}${parts.length > 0 ? ` (${joinParts(parts)})` : ''}.`);
  }
  if (withdrawn > 0) sentences.push(`Withdrew ${plural(withdrawn, 'incoming message', 'incoming messages')}.`);
  if (ended > 0) sentences.push(`Already finished: ${plural(ended, 'agent', 'agents')}.`);
  if (delivered > 0) sentences.push(`Already delivered: ${plural(delivered, 'incoming message', 'incoming messages')}.`);
  if (failures.length > 0) sentences.push(`Not stopped: ${failures.join('; ')}`);
  else if (failed > 0) sentences.push(`${plural(failed, 'stop', 'stops')} failed.`);
  const tone: ToastType = failed > 0 && stopped + withdrawn === 0
    ? 'error'
    : failures.length > 0 || failed > 0 ? 'warning' : stopped + withdrawn > 0 ? 'success' : 'info';
  return { tone, text: sentences.join(' ') };
}
