<script lang="ts">
  // A finished reply's read-aloud control, in the meta row beside Copy and
  // in its grid (the same IconButton size). Idle reads the reply; playing
  // pauses; paused resumes from the start of its sentence, with a restart
  // beside it. While an OpenAI voice is fetching the first sentence the
  // control shows a spinner instead of pause. The store
  // (`stores/readAloud.svelte.ts`) owns the voice and the one reading at a
  // time; `backend` is the computer the reply belongs to, or null when it
  // is not verified.

  import Pause from '@lucide/svelte/icons/pause';
  import Play from '@lucide/svelte/icons/play';
  import RotateCcw from '@lucide/svelte/icons/rotate-ccw';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import type { BackendKey } from '../../transport/backendKey';
  import { chordHintSuffix } from '../../stores/keybindings.svelte';
  import {
    pauseReadAloud,
    readAloud,
    readAloudPhase,
    readAloudWaiting,
    restartReadAloud,
    resumeReadAloud,
  } from '../../stores/readAloud.svelte';

  let { readingKey, markdown, backend }: { readingKey: string; markdown: string; backend: BackendKey | null } = $props();

  let phase = $derived(readAloudPhase(readingKey));
  let waiting = $derived(readAloudWaiting(readingKey));
  let label = $derived(
    phase === 'playing'
      ? `Pause reading${chordHintSuffix('readAloud.toggle')}`
      : phase === 'paused'
        ? `Resume reading${chordHintSuffix('readAloud.toggle')}`
        : 'Read aloud',
  );

  function toggle(): void {
    if (phase === 'playing') pauseReadAloud();
    else if (phase === 'paused') resumeReadAloud();
    else readAloud(readingKey, markdown, backend);
  }
</script>

<span
  data-testid="assistant-message-read-aloud-slot"
  data-phase={phase}
  data-waiting={waiting}
  class="flex shrink-0 items-center"
>
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
