<script lang="ts">
  // The composer's mic: starts and stops one DictationSession. Owns only the
  // trigger chrome; the session owns the recording and the composer owns
  // where the transcript goes. An outlined circle of Send's diameter: on the
  // desktop it sits in the toolbar immediately left of Send, on the phone in
  // the input row in Send's column (the host decides; see Composer).

  import Mic from '@lucide/svelte/icons/mic';
  import Square from '@lucide/svelte/icons/square';
  import Icon from '../primitives/Icon.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import { chordHintSuffix } from '../../stores/keybindings.svelte';
  import type { DictationSession } from '../../dictation/dictationSession.svelte';

  interface Props {
    session: DictationSession;
  }

  let { session }: Props = $props();

  let phase = $derived(session.phase);
  let recording = $derived(phase === 'recording');
  let busy = $derived(phase === 'starting' || phase === 'transcribing');
  let shortcut = $derived(chordHintSuffix('composer.dictate'));
  let label = $derived(
    recording ? 'Stop dictation'
      : phase === 'transcribing' ? 'Transcribing…'
        : phase === 'starting' ? 'Starting microphone…'
          : `Dictate${shortcut}`,
  );
  let title = $derived(session.error ?? label);

  // A tap must not blur the textarea and drop the phone keyboard, for the
  // same reason as the Send button.
  function keepInputFocus(event: PointerEvent): void {
    if (event.pointerType !== 'mouse') event.preventDefault();
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape' && phase !== 'idle') {
      event.preventDefault();
      session.cancel();
    }
  }
</script>

<button
  type="button"
  data-testid="composer-dictate"
  data-phase={phase}
  data-level={recording ? session.level : undefined}
  data-error={session.error ? 'true' : undefined}
  aria-label={label}
  aria-pressed={recording}
  {title}
  disabled={busy}
  onpointerdown={keepInputFocus}
  onkeydown={handleKeydown}
  onclick={() => session.toggle()}
  class="dictation-button relative inline-flex size-8 compact:size-10 shrink-0 items-center justify-center rounded-full focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 disabled:cursor-default
    {recording
      ? 'bg-error text-surface-0 hover:bg-error/85'
      : session.error
        ? 'border border-error/60 text-error hover:bg-surface-2/40'
        : 'border border-border-subtle text-fg-muted hover:bg-surface-2/40 hover:text-fg'}"
>
  {#if recording}
    <span aria-hidden="true" class="dictation-level pointer-events-none absolute inset-0 rounded-full"></span>
    <Icon icon={Square} size={12} strokeWidth={3} class="fill-current" />
  {:else if phase === 'transcribing'}
    <SteppedSpinner size={14} />
  {:else}
    <Icon icon={Mic} size={16} strokeWidth={2} />
  {/if}
</button>

<style>
  /* The loudness ring steps with the session's level (0-4), so it repaints
     at most ten times a second and never animates between steps. It tops
     out at 6px: the attachment picker sits one 4px gap away, and a wider
     halo would wash over it. */
  .dictation-level {
    box-shadow: 0 0 0 0 color-mix(in oklab, var(--color-error) 35%, transparent);
  }
  .dictation-button[data-level='1'] .dictation-level { box-shadow: 0 0 0 1.5px color-mix(in oklab, var(--color-error) 35%, transparent); }
  .dictation-button[data-level='2'] .dictation-level { box-shadow: 0 0 0 3px color-mix(in oklab, var(--color-error) 35%, transparent); }
  .dictation-button[data-level='3'] .dictation-level { box-shadow: 0 0 0 4.5px color-mix(in oklab, var(--color-error) 35%, transparent); }
  .dictation-button[data-level='4'] .dictation-level { box-shadow: 0 0 0 6px color-mix(in oklab, var(--color-error) 35%, transparent); }
</style>
