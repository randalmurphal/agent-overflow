// Running an intercepted composer command.
//
// These commands are consumed at send: the draft text never reaches a provider
// and is never persisted as a message. Each one routes into the affordance
// that already exists — the model picker, the effort menu, the settings
// surface, the new-thread action, the pane title's inline rename — rather than
// growing a parallel implementation. `/compact` and `/review` are the two that
// have no existing UI affordance, and they call their bindings directly.
//
// Every failure comes back as a string for the caller to render next to the
// composer. Nothing here toasts: the user is looking at the text they typed,
// which is where the answer belongs.

import { CompactCodexThread, StartCodexReview } from '../../stores/bindings';
import { makeCommandContext } from '../../stores/builtinCommands.svelte';
import { runCommand } from '../../stores/commandRegistry.svelte';
import { openComposerPicker } from '../../stores/composerPickerRegistry.svelte';
import { openSettingsOverlay } from '../../stores/settingsOverlay.svelte';
import { getProviderModels } from '../../stores/providerModels.svelte';
import { getSettings } from '../../stores/settings.svelte';
import { renameThreadTitle, startPaneTitleRename } from '../../stores/paneTitleRename';
import { getActiveTurn } from '../../stores/threadStatuses.svelte';
import {
  applyThreadFastMode,
  applyThreadModelSelection,
  applyThreadReasoningEffort,
} from '../../stores/threadModelControls';
import type { ThreadPane } from '../../stores/thread.svelte';
import { asProviderID } from '../../types/providers';
import type { ModelInfo, ReasoningEffortOption } from '../../types/settings';
import { pickerVisibleModels } from '../../utils/hiddenModels';
import { displayModelLabel } from '../../utils/modelLabels';
import { errString } from '../../utils/errors';
import { resolveArgCandidate, type ArgCandidate } from './composerCommandArgs';
import type { InterceptedInvocation } from './composerCommandParse';
import { parseReviewTarget } from './composerReviewTargets';

/** Effort tiers a model with no catalog entry still offers, per EffortMenu. */
const FALLBACK_EFFORTS: readonly ReasoningEffortOption[] = [
  { slug: 'low', label: 'Low' },
  { slug: 'medium', label: 'Medium' },
  { slug: 'high', label: 'High' },
  { slug: 'xhigh', label: 'xHigh' },
];

export interface CommandActionResult {
  /** User-facing failure text. Empty on success. */
  error: string;
}

const DONE: CommandActionResult = { error: '' };

function fail(error: string): CommandActionResult {
  return { error };
}

function activeModelInfo(pane: ThreadPane): ModelInfo | undefined {
  const provider = asProviderID(pane.thread?.provider);
  if (!provider) return undefined;
  return getProviderModels(provider).find((model) => model.slug === (pane.thread?.model ?? ''));
}

function modelCandidates(pane: ThreadPane): ArgCandidate[] {
  const provider = asProviderID(pane.thread?.provider);
  if (!provider) return [];
  const models = pickerVisibleModels(
    getSettings(),
    provider,
    getProviderModels(provider),
    pane.activeModel,
  );
  return models.map((model) => ({
    id: model.slug,
    label: displayModelLabel(provider, model.slug, model.name),
  }));
}

function effortCandidates(pane: ThreadPane): ArgCandidate[] {
  // Same distinction the effort menu draws: no catalog entry is IGNORANCE (use
  // the generic tiers), an entry with no tiers is KNOWLEDGE (this model has
  // none), and offering tiers a model does not have is the lie to avoid.
  const info = activeModelInfo(pane);
  const options = info ? (info.reasoningEfforts ?? []) : FALLBACK_EFFORTS;
  return options.map((option) => ({ id: option.slug, label: option.label || option.slug }));
}

async function runModel(pane: ThreadPane, arg: string): Promise<CommandActionResult> {
  if (arg === '') {
    return openComposerPicker(pane.paneId, 'model')
      ? DONE
      : fail('The model picker is not available here.');
  }
  const provider = asProviderID(pane.thread?.provider);
  if (!provider) return fail('Open a thread before switching models.');
  const resolved = resolveArgCandidate(arg, modelCandidates(pane), 'model');
  if (!resolved.id) return fail(resolved.error ?? 'No model matched.');
  const result = await applyThreadModelSelection(pane, provider, resolved.id);
  return result.ok ? DONE : fail(result.error ?? 'Failed to switch model.');
}

async function runEffort(pane: ThreadPane, arg: string): Promise<CommandActionResult> {
  if (arg === '') {
    return openComposerPicker(pane.paneId, 'effort')
      ? DONE
      : fail('This model has no options to choose.');
  }
  const candidates = effortCandidates(pane);
  if (candidates.length === 0) {
    return fail('This model does not support reasoning effort tiers.');
  }
  const resolved = resolveArgCandidate(arg, candidates, 'effort tier');
  if (!resolved.id) return fail(resolved.error ?? 'No effort tier matched.');
  const result = await applyThreadReasoningEffort(pane, resolved.id);
  return result.ok ? DONE : fail(result.error ?? 'Failed to set effort.');
}

async function runFast(pane: ThreadPane): Promise<CommandActionResult> {
  const info = activeModelInfo(pane);
  // Support is a catalog capability, never the tier metadata. An unknown model
  // (catalog not loaded, or a model we have no row for) is unknown, not
  // unsupported — say so rather than silently doing nothing.
  if (!info) {
    return fail('Model capabilities are still loading; try again in a moment.');
  }
  if (!(info.capabilities ?? []).includes('fast_mode')) {
    const label = displayModelLabel(pane.thread?.provider ?? '', info.slug, info.name);
    return fail(`${label} does not support fast mode.`);
  }
  const result = await applyThreadFastMode(pane, pane.thread?.fastMode !== true);
  return result.ok ? DONE : fail(result.error ?? 'Failed to toggle fast mode.');
}

function runConfig(): CommandActionResult {
  openSettingsOverlay('general');
  return DONE;
}

function runClear(pane: ThreadPane): CommandActionResult {
  // `thread.new` is the same action the palette and the sidebar's + button
  // run; it resolves the project from the focused pane, which is the pane
  // whose composer just consumed this command.
  return runCommand('thread.new', makeCommandContext(pane, {}))
    ? DONE
    : fail('Could not start a new thread.');
}

async function runRename(pane: ThreadPane, arg: string): Promise<CommandActionResult> {
  const threadId = pane.threadId;
  if (!threadId) return fail('Start the thread before renaming it.');
  if (arg === '') {
    return startPaneTitleRename(pane.paneId)
      ? DONE
      : fail('The thread title is not editable here.');
  }
  const result = await renameThreadTitle(threadId, arg, pane.thread?.title);
  return result.ok ? DONE : fail(result.error ?? 'Failed to rename thread.');
}

/**
 * Both Codex turn-starting commands need an idle thread: compaction and review
 * run as NON-STEERABLE turns, so queuing one behind a live turn would either
 * be dropped or collide with it.
 */
function requireIdleCodexThread(pane: ThreadPane, what: string): string {
  if ((pane.thread?.provider ?? '') !== 'codex') return `${what} is a Codex-only command.`;
  if (!pane.threadId) return `Send a message before running ${what}.`;
  if (getActiveTurn(pane.threadId) !== null) {
    return `${what} needs an idle thread — wait for the current turn to finish.`;
  }
  return '';
}

async function runCompact(pane: ThreadPane): Promise<CommandActionResult> {
  const blocked = requireIdleCodexThread(pane, '/compact');
  if (blocked !== '') return fail(blocked);
  try {
    await CompactCodexThread(pane.threadId!);
    return DONE;
  } catch (err) {
    return fail(`Failed to compact: ${errString(err)}`);
  }
}

async function runReview(pane: ThreadPane, arg: string): Promise<CommandActionResult> {
  const blocked = requireIdleCodexThread(pane, '/review');
  if (blocked !== '') return fail(blocked);
  const parsed = parseReviewTarget(arg);
  if (!parsed.target) return fail(parsed.error ?? 'Could not read the review target.');
  try {
    await StartCodexReview(pane.threadId!, parsed.target);
    return DONE;
  } catch (err) {
    return fail(`Failed to start review: ${errString(err)}`);
  }
}

/**
 * Run one intercepted command. The caller has already consumed the composer
 * text; this only performs the action and reports whether it worked.
 */
export async function runInterceptedCommand(
  pane: ThreadPane,
  invocation: InterceptedInvocation,
): Promise<CommandActionResult> {
  switch (invocation.name) {
    case 'model':
      return runModel(pane, invocation.arg);
    case 'effort':
      return runEffort(pane, invocation.arg);
    case 'fast':
      return runFast(pane);
    case 'config':
      return runConfig();
    case 'clear':
      return runClear(pane);
    case 'rename':
      return runRename(pane, invocation.arg);
    case 'compact':
      return runCompact(pane);
    case 'review':
      return runReview(pane, invocation.arg);
    default:
      // Unreachable while the registry and this switch agree; a name added to
      // one and not the other must be loud rather than silently sent.
      return fail(`Unknown command /${invocation.name}.`);
  }
}
