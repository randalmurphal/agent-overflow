import { liveCodexAgent } from '../stores/subagentProgress.svelte';
import type { Item } from '../types/models';
import { parseJsonObject } from './parseJsonObject';
import { isCodexSubagentLaunchItem } from './subagentLaunch';

/** View of current execution; the historical spawn and deliveries stay intact. */
export function subagentExecutionItem(launch: Item, completion?: Item | null): Item;
export function subagentExecutionItem(launch: Item | undefined, completion?: Item | null): Item | undefined;
export function subagentExecutionItem(launch: Item | undefined, completion?: Item | null): Item | undefined {
  if (!launch || !isCodexSubagentLaunchItem(launch)) return completion ?? launch;
  const live = liveCodexAgent(launch.threadId, launch.id);
  const meta = parseJsonObject((live ?? launch).meta);
  if (!live && meta?.codex_live_projection !== true) return completion ?? launch;
  launch = live ?? launch;
  const runtime = meta?.codex_runtime as Record<string, unknown> | undefined;
  const active = meta?.live_background_active;
  if (!runtime && typeof active !== 'boolean') return completion ?? launch;
  const ended = typeof meta?.codex_background_end_reason === 'string';
  const running = !ended && active === true;
  const rawStatus = runtime?.status;
  const status: Item['status'] = running ? 'running'
    : rawStatus === 'errored' || rawStatus === 'systemError' ? 'errored'
    : ended || rawStatus === 'interrupted' || rawStatus === 'shutdown' || rawStatus === 'notLoaded' || rawStatus === 'notFound' ? 'killed'
    : 'completed';
  return { ...launch, status, isBackground: running,
    createdAt: typeof runtime?.startedAt === 'number' ? runtime.startedAt : launch.createdAt,
    updatedAt: typeof runtime?.updatedAt === 'number' && !ended ? runtime.updatedAt : launch.updatedAt };
}
