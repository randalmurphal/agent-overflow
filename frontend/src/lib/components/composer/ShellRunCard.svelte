<script lang="ts">
  // The composer shell command running for this thread, above the composer:
  // the command, how long it has run, the end of its output and Stop. When
  // the command exits the card goes away and the message it became arrives
  // through the timeline or the send queue; a run whose output could not be
  // sent stays as an error until dismissed.
  //
  // A command that prompts on its own terminal (sudo, ssh) shows the prompt
  // and a field to answer it, while the terminal waits on an open line
  // (shellTerminalPrompt). The answer is often a password: it is read from
  // the field, cleared from it and passed to answerShellCommand, and held
  // nowhere else.
  import { untrack } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    answerShellCommand,
    dismissShellRun,
    getShellRun,
    stopShellCommand,
  } from '../../stores/shellCommands.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { shellOutputTail, shellTerminalPrompt } from '../../utils/shellInvocation';
  import { errString } from '../../utils/errors';
  import { findPaneComposer, focusPaneComposer } from '../panes/paneComposerFocus';
  import AnsiText from '../chat/AnsiText.svelte';
  import { createRunningElapsed } from '../chat/useRunningElapsed.svelte';
  import Button from '../primitives/Button.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();
  const promptId = $props.id();

  /** Lines of live output the card shows. The whole output is in the message. */
  const TAIL_LINES = 6;
  /** Lines of the command's terminal the prompt shows. */
  const PROMPT_LINES = 3;

  const run = $derived(getShellRun(pane.threadId));
  const running = $derived(run?.state === 'running');
  const tail = $derived(run && running ? shellOutputTail(run.output, TAIL_LINES) : '');
  const elapsed = createRunningElapsed(() => running, () => run?.startedAt ?? 0, 1_000);
  const prompt = $derived(run && running ? shellTerminalPrompt(run.terminal, PROMPT_LINES) : '');
  const asking = $derived(prompt !== '');

  let answerRow = $state<HTMLElement | null>(null);
  let answerInput = $state<HTMLInputElement | null>(null);
  let revealed = $state(false);
  // The run whose prompt this card is showing, so focus is offered once per
  // appearance of a prompt and not on every terminal frame.
  let promptRunId = '';

  // A prompt appears right after Enter in the composer, so the next
  // keystrokes, often a password, would land in the composer and be saved
  // with its draft. Take focus from the composer or from nowhere; never
  // from another surface.
  $effect(() => {
    const runId = asking && run ? run.runId : '';
    if (runId === promptRunId) return;
    promptRunId = runId;
    if (runId === '') return;
    untrack(() => {
      const active = document.activeElement;
      const fromComposer = active !== null && active === findPaneComposer(pane.paneId);
      if (active === null || active === document.body || fromComposer) {
        answerInput?.focus({ preventScroll: true });
      }
    });
  });

  // The answer row goes away when the prompt is answered or the run ends.
  // Before it leaves the DOM, focus inside it moves to the composer so it
  // never falls to the document.
  $effect.pre(() => {
    if (asking) return;
    untrack(() => {
      revealed = false;
      if (answerRow?.contains(document.activeElement)) focusPaneComposer(pane.paneId);
    });
  });

  async function answer(): Promise<void> {
    const threadId = pane.threadId;
    const input = answerInput;
    if (!threadId || !input) return;
    // Taken out of the field before anything awaits, so the field is empty
    // and masked again while the answer is in flight, and a failure cannot
    // put it back.
    const text = input.value;
    input.value = '';
    revealed = false;
    try {
      await answerShellCommand(threadId, text);
    } catch (err) {
      pane.setGeneralError(`Failed to send the answer: ${errString(err)}`, err);
    }
  }

  function onAnswerKeydown(e: KeyboardEvent): void {
    if (e.key !== 'Enter' || e.isComposing) return;
    e.preventDefault();
    void answer();
  }

  function toggleReveal(): void {
    revealed = !revealed;
    answerInput?.focus({ preventScroll: true });
  }

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
    <div class="flex min-h-8 items-center gap-2 pl-3 pr-1.5">
      {#if running}
        <SteppedSpinner size={12} class="shrink-0 text-fg-subtle" animate={!getSettings().lowPowerMode} />
      {/if}
      <span class="select-none font-mono text-[0.75rem] text-accent" aria-hidden="true">$</span>
      <!-- While the command asks, all of it shows: it says which command
           wants the answer, and a code block's Run can start one the person
           did not type. -->
      <code
        class="min-w-0 flex-1 font-mono text-[0.75rem] text-fg {asking ? 'max-h-[9rem] overflow-y-auto whitespace-pre-wrap wrap-anywhere py-1.5' : 'truncate'}"
        title={run.command}
        data-testid="shell-run-command"
      >{run.command}</code>
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
    {/if}
    {#if asking}
      <div class="border-t border-border-subtle px-3 py-1.5" data-testid="shell-run-prompt">
        <p class="text-[0.6875rem] font-medium text-fg-muted">The command is asking:</p>
        <div class="flex max-h-[4.5rem] flex-col justify-end overflow-hidden" id={promptId}>
          <AnsiText source={prompt} class="whitespace-pre-wrap wrap-anywhere text-[0.6875rem] leading-[1.45] text-fg" />
        </div>
        <div class="mt-1.5 flex items-center gap-1.5" bind:this={answerRow}>
          <input
            bind:this={answerInput}
            type={revealed ? 'text' : 'password'}
            class="h-6 min-w-0 flex-1 rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-2 font-mono text-[0.75rem] text-fg focus:border-accent focus:outline-none compact:h-9 compact:text-base"
            aria-label="Answer the command's prompt"
            aria-describedby={promptId}
            autocomplete="off"
            autocapitalize="off"
            autocorrect="off"
            spellcheck={false}
            enterkeyhint="send"
            data-1p-ignore
            data-lpignore="true"
            data-bwignore
            data-testid="shell-run-answer"
            onkeydown={onAnswerKeydown}
          />
          <Button
            variant="ghost"
            size="xs"
            pressed={revealed}
            ariaLabel={revealed ? 'Hide the answer' : 'Show the answer'}
            onclick={toggleReveal}
            testId="shell-run-answer-reveal"
          >
            {revealed ? 'Hide' : 'Show'}
          </Button>
          <Button variant="secondary" size="xs" onclick={() => void answer()} testId="shell-run-answer-send">
            Send
          </Button>
        </div>
      </div>
    {/if}
    {#if !running}
      <p class="border-t border-border-subtle px-3 py-1.5 text-[0.6875rem] text-error" role="alert" data-testid="shell-run-error">
        {run.error}
      </p>
    {/if}
  </div>
{/if}
