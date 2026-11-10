<script lang="ts">
  // The composer shell command running for this thread, above the composer:
  // the command, how long it has run, the end of its output and Stop. When
  // the command exits the card goes away and the message it became arrives
  // through the timeline or the send queue; a run whose output could not be
  // sent stays as an error until dismissed.
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    dismissShellRun,
    getShellRun,
    stopShellCommand,
  } from '../../stores/shellCommands.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { shellOutputTail } from '../../utils/shellInvocation';
  import { errString } from '../../utils/errors';
  import { focusPaneComposer } from '../panes/paneComposerFocus';
  import AnsiText from '../chat/AnsiText.svelte';
  import { createRunningElapsed } from '../chat/useRunningElapsed.svelte';
  import Button from '../primitives/Button.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  /** Lines of live output the card shows. The whole output is in the message. */
  const TAIL_LINES = 6;

  const run = $derived(getShellRun(pane.threadId));
  const running = $derived(run?.state === 'running');
  const tail = $derived(run && running ? shellOutputTail(run.output, TAIL_LINES) : '');
  const elapsed = createRunningElapsed(() => running, () => run?.startedAt ?? 0, 1_000);

  // The card's buttons go away with the card. Focus moves to the composer
  // first, so it never falls to the document.
  async function stop(): Promise<void> {
    const threadId = pane.threadId;
    if (!threadId) return;
    focusPaneComposer(pane.paneId);
    try {
      await stopShellCommand(threadId);
    } catch (err) {
      pane.setGeneralError(`Failed to stop the command: ${errString(err)}`, err);
    }
  }

  function dismiss(): void {
    if (!pane.threadId) return;
    focusPaneComposer(pane.paneId);
    dismissShellRun(pane.threadId);
  }
</script>

{#if run && pane.threadId}
  <div
    class="mb-2 overflow-hidden rounded-lg border bg-card shadow-sheet {running ? 'border-border' : 'border-error/40'}"
    role="group"
    aria-label={running ? 'Running command' : 'Command failed'}
    data-testid="shell-run-card"
    data-state={run.state}
  >
    <div class="flex h-8 items-center gap-2 pl-3 pr-1.5">
      {#if running}
        <SteppedSpinner size={12} class="shrink-0 text-fg-subtle" animate={!getSettings().lowPowerMode} />
      {/if}
      <span class="select-none font-mono text-[0.75rem] text-accent" aria-hidden="true">$</span>
      <code class="min-w-0 flex-1 truncate font-mono text-[0.75rem] text-fg" title={run.command} data-testid="shell-run-command">{run.command}</code>
      {#if running}
        <span class="shrink-0 text-[0.6875rem] tabular-nums text-fg-hint" data-testid="shell-run-elapsed">{elapsed.label}</span>
        <Button variant="ghost" size="xs" disabled={run.stopping} onclick={() => void stop()} testId="shell-run-stop">
          {run.stopping ? 'Stopping…' : 'Stop'}
        </Button>
      {:else}
        <Button variant="ghost" size="xs" onclick={dismiss} testId="shell-run-dismiss">
          Dismiss
        </Button>
      {/if}
    </div>
    {#if running && tail !== ''}
      <!-- Bottom-anchored and clipped: new lines push old ones out of the top
           and the card stops growing at TAIL_LINES. -->
      <div
        class="flex max-h-[6.75rem] flex-col justify-end overflow-hidden border-t border-border-subtle bg-surface-0/40 px-3 py-1.5"
        data-testid="shell-run-output"
      >
        <AnsiText source={tail} class="whitespace-pre text-[0.6875rem] leading-[1.45] text-fg-muted" />
      </div>
    {:else if !running}
      <p class="border-t border-border-subtle px-3 py-1.5 text-[0.6875rem] text-error" role="alert" data-testid="shell-run-error">
        {run.error}
      </p>
    {/if}
  </div>
{/if}
