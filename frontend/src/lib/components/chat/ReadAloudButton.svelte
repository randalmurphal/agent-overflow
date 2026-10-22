<script lang="ts">
  // A finished reply's read-aloud control, in the meta row beside Copy and
  // in its grid (the same IconButton size). Idle reads the reply; playing
  // pauses; paused resumes from the start of its sentence, with a restart
  // beside it. The store (`stores/readAloud.svelte.ts`) owns the voice and
  // the one reading at a time.

  import Pause from '@lucide/svelte/icons/pause';
  import Play from '@lucide/svelte/icons/play';
  import RotateCcw from '@lucide/svelte/icons/rotate-ccw';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import { chordHintSuffix } from '../../stores/keybindings.svelte';
  import {
    pauseReadAloud,
    readAloud,
    readAloudPhase,
    restartReadAloud,
    resumeReadAloud,
  } from '../../stores/readAloud.svelte';

  let { readingKey, markdown }: { readingKey: string; markdown: string } = $props();

  let phase = $derived(readAloudPhase(readingKey));
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
    else readAloud(readingKey, markdown);
  }
</script>

<span
  data-testid="assistant-message-read-aloud-slot"
  data-phase={phase}
  class="flex shrink-0 items-center"
>
  <IconButton
    {label}
    size="sm"
    testId="assistant-message-read-aloud"
    onClick={toggle}
  >
    {#snippet children()}
      <Icon icon={phase === 'playing' ? Pause : Play} size={13} />
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
