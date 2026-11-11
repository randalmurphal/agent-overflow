<script lang="ts">
  // A finished reply's read-aloud controls, in the meta row beside Copy and
  // in its grid (the same IconButton size). Idle shows one play button.
  // While this reply is being read or is paused it shows, in order:
  // previous section, back (READ_ALOUD_BACK_SECONDS, or the sentence's
  // start for a device voice), play / pause / resume, next section, and
  // "Read from the start" while paused. Playing pauses; paused resumes
  // where it paused (an OpenAI voice mid-clip, a device voice at the start
  // of its sentence). While an OpenAI voice is fetching the section it is
  // waiting for, the control shows a spinner instead of pause. The store
  // (`stores/readAloud.svelte.ts`) owns the voice and the one reading at a
  // time, and reads through the computer `threadId` verifiably runs on.

  import Pause from '@lucide/svelte/icons/pause';
  import Play from '@lucide/svelte/icons/play';
  import Rewind from '@lucide/svelte/icons/rewind';
  import RotateCcw from '@lucide/svelte/icons/rotate-ccw';
  import SkipBack from '@lucide/svelte/icons/skip-back';
  import SkipForward from '@lucide/svelte/icons/skip-forward';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import { chordHintSuffix } from '../../stores/keybindings.svelte';
  import {
    READ_ALOUD_BACK_SECONDS,
    pauseReadAloud,
    readAloud,
    readAloudBack,
    readAloudBackStep,
    readAloudNextSection,
    readAloudPhase,
    readAloudPosition,
    readAloudPreviousSection,
    readAloudWaiting,
    restartReadAloud,
    resumeReadAloud,
  } from '../../stores/readAloud.svelte';

  let { readingKey, markdown, threadId }: { readingKey: string; markdown: string; threadId: string } = $props();

  let phase = $derived(readAloudPhase(readingKey));
  let waiting = $derived(readAloudWaiting(readingKey));
  let position = $derived(readAloudPosition(readingKey));
  let backStep = $derived(position ? readAloudBackStep() : null);
  let label = $derived(
    phase === 'playing'
      ? `Pause reading${chordHintSuffix('readAloud.toggle')}`
      : phase === 'paused'
        ? `Resume reading${chordHintSuffix('readAloud.toggle')}`
        : 'Read aloud',
  );
  let backLabel = $derived(backStep === 'seconds' ? `Back ${READ_ALOUD_BACK_SECONDS} seconds` : 'Repeat this sentence');
  // A paused device voice is already at its sentence's start, so back has
  // nothing to do there.
  let backDisabled = $derived(waiting || (backStep === 'sentence' && phase === 'paused'));
  let lastSection = $derived(position !== null && position.section >= position.sections - 1);

  function toggle(): void {
    if (phase === 'playing') pauseReadAloud();
    else if (phase === 'paused') resumeReadAloud();
    else readAloud(readingKey, markdown, { thread: threadId });
  }
</script>

<span
  data-testid="assistant-message-read-aloud-slot"
  data-phase={phase}
  data-waiting={waiting}
  class="flex shrink-0 items-center"
>
  {#if position}
    <IconButton
      label="Previous section"
      size="sm"
      testId="assistant-message-read-aloud-previous"
      onClick={readAloudPreviousSection}
    >
      {#snippet children()}
        <Icon icon={SkipBack} size={13} />
      {/snippet}
    </IconButton>
    <IconButton
      label={backLabel}
      size="sm"
      disabled={backDisabled}
      testId="assistant-message-read-aloud-back"
      onClick={readAloudBack}
    >
      {#snippet children()}
        <Icon icon={Rewind} size={13} />
      {/snippet}
    </IconButton>
  {/if}
  <IconButton
    {label}
    size="sm"
    testId="assistant-message-read-aloud"
    onClick={toggle}
  >
    {#snippet children()}
      {#if waiting}
        <SteppedSpinner size={13} />
      {:else}
        <Icon icon={phase === 'playing' ? Pause : Play} size={13} />
      {/if}
    {/snippet}
  </IconButton>
  {#if position}
    <IconButton
      label="Next section"
      size="sm"
      disabled={lastSection}
      testId="assistant-message-read-aloud-next"
      onClick={readAloudNextSection}
    >
      {#snippet children()}
        <Icon icon={SkipForward} size={13} />
      {/snippet}
    </IconButton>
  {/if}
  {#if phase === 'paused'}
    <IconButton
      label="Read from the start"
      size="sm"
      testId="assistant-message-read-aloud-restart"
      onClick={restartReadAloud}
    >
      {#snippet children()}
        <Icon icon={RotateCcw} size={13} />
      {/snippet}
    </IconButton>
  {/if}
</span>
