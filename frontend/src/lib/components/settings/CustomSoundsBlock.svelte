<script lang="ts">
  // Settings → Notifications → Sounds: the custom cue library.
  //
  // The library belongs to the BACKEND HOST (<configDir>/sounds), not to this
  // screen: every screen attached to that computer offers the same cues, and
  // the host re-validates every file on every listing. So this block edits a
  // directory it does not own, and shows what that host says about it —
  // warnings included — rather than tracking anything locally.
  //
  // NOTHING THE USER PICKS IS STORED AS PICKED. The file goes to the engine's
  // own sandboxed decoder and is re-rendered to a canonical WAV here
  // (lib/audio/renderCue.ts); Go then re-checks those bytes before writing.
  // A failure on any leg is shown in this block, never console-only.
  //
  // Extracted from NotificationsSection for the reason PhonePushBlock was:
  // the section is three stacks of preference rows, and this is a small
  // editor for a resource that is not a preference at all.

  import Trash2 from '@lucide/svelte/icons/trash-2';
  import Volume2 from '@lucide/svelte/icons/volume-2';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import SettingsCallout from './SettingsCallout.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { playNotificationCue } from '../../stores/notificationSound';
  import {
    addCustomSound,
    customSoundsError,
    deleteCustomSound,
    ensureCustomSounds,
    peekCustomSounds,
  } from '../../stores/sounds.svelte';
  import { renderCueWav } from '../../audio/renderCue';
  import { userFacingError } from '../../utils/userFacingError';

  /** The prefix a cue value carries when it names a file in the library. */
  const CUSTOM_PREFIX = 'custom:';

  /** soundlib's id grammar is `[a-z0-9][a-z0-9-]{0,63}`: 64 characters. */
  const MAX_SOUND_ID = 64;

  const DESCRIPTION =
    'Your own cues, kept on this computer and offered to every screen attached to it.';

  // Every failure this block can produce: a file the engine cannot decode,
  // one past the duration cap, a name already taken, a directory the host
  // cannot write.
  let libraryError = $state('');
  let adding = $state(false);
  // The cue a delete is waiting on confirmation for, or null.
  let pendingDelete: string | null = $state(null);
  let fileInput: HTMLInputElement | undefined = $state();

  $effect(() => {
    ensureCustomSounds();
  });

  let library = $derived(peekCustomSounds());

  /**
   * How many of this screen's three events play the cue being deleted. The
   * confirm says so because the file is the only copy: a deleted cue is gone
   * from every screen attached to this computer, and the events still
   * naming it fall back to their defaults.
   */
  function eventsUsing(id: string): number {
    const settings = getSettings();
    const cue = `${CUSTOM_PREFIX}${id}`;
    return [
      settings.notifySoundCueTurnComplete,
      settings.notifySoundCueInputNeeded,
      settings.notifySoundCueAttention,
    ].filter((value) => value === cue).length;
  }

  function deleteDescription(id: string): string {
    const used = eventsUsing(id);
    const base = `Remove ${id} from this computer's sound library. Every screen attached to it loses the cue.`;
    if (used === 0) return base;
    return `${base} ${used === 1 ? 'One event' : `${used} events`} on this screen play it now and will play the default sound instead.`;
  }

  /**
   * The id a picked file is stored under: its stem as kebab-case ASCII.
   *
   * The backend's grammar is the constraint, not a preference — soundlib
   * refuses anything else — so accents are folded rather than dropped, every
   * other run becomes one dash, and a name with nothing left of it ("♪.wav")
   * still gets a legal id instead of a refusal the user cannot act on.
   */
  function idForFile(name: string): string {
    const stem = name.replace(/\.[^.]+$/, '');
    const id = stem
      .normalize('NFKD')
      .replace(/\p{M}+/gu, '')
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, '-')
      .slice(0, MAX_SOUND_ID)
      .replace(/^-+|-+$/g, '');
    return id === '' ? 'sound' : id;
  }

  async function addPickedFile(): Promise<void> {
    const file = fileInput?.files?.[0];
    // Cleared before the await so picking the same file twice in a row still
    // fires a change event the second time.
    if (fileInput) fileInput.value = '';
    if (!file) return;
    libraryError = '';
    adding = true;
    try {
      const rendered = await renderCueWav(file);
      await addCustomSound(idForFile(file.name), rendered.wav);
    } catch (cause) {
      libraryError = userFacingError(cause, 'Could not add that sound.');
    } finally {
      adding = false;
    }
  }

  async function removeSound(id: string): Promise<void> {
    libraryError = '';
    try {
      await deleteCustomSound(id);
    } catch (cause) {
      libraryError = userFacingError(cause, `Could not delete ${id}.`);
    }
  }
</script>

<div
  class="pt-3"
  data-settings-field="notifications.custom-sounds"
  data-settings-label="Custom sounds"
  data-settings-hint={DESCRIPTION}
>
  <SettingsHeader title="Custom sounds" description={DESCRIPTION} />
  <div class="flex flex-col gap-1">
    {#if library.sounds.length > 0}
      <ul class="flex flex-col gap-0.5" data-testid="settings-sound-library">
        {#each library.sounds as sound (sound.id)}
          <li class="flex items-center gap-2 rounded-[var(--radius-field)] px-1 py-0.5">
            <span class="min-w-0 flex-1 truncate font-mono text-[0.8125rem] text-fg">
              {sound.id}
            </span>
            <IconButton
              label={`Play ${sound.id}`}
              size="sm"
              onClick={() => playNotificationCue(`${CUSTOM_PREFIX}${sound.id}`)}
              testId={`settings-sound-play-${sound.id}`}
            >
              <Icon icon={Volume2} size={14} strokeWidth={2} />
            </IconButton>
            <IconButton
              label={`Delete ${sound.id}`}
              size="sm"
              onClick={() => {
                pendingDelete = sound.id;
              }}
              testId={`settings-sound-delete-${sound.id}`}
            >
              <Icon icon={Trash2} size={14} strokeWidth={2} />
            </IconButton>
          </li>
        {/each}
      </ul>
    {/if}

    <div class="flex items-center gap-2 px-1 pt-1">
      <Button
        variant="secondary"
        disabled={adding}
        loading={adding}
        onclick={() => fileInput?.click()}
        testId="settings-sound-add"
      >
        Add sound
      </Button>
      <span class="text-[0.75rem] text-fg-hint">
        Any audio file, up to 3 seconds. Converted here before it is saved.
      </span>
    </div>
    <!-- `accept` is a file-picker filter, never a check: the engine's decoder
         decides what this is, and the host re-checks the bytes this page
         produces. -->
    <input
      bind:this={fileInput}
      type="file"
      accept="audio/*"
      hidden
      aria-label="Choose a sound file"
      data-testid="settings-sound-file"
      onchange={() => void addPickedFile()}
    />

    {#if libraryError}
      <SettingsCallout tone="error">{libraryError}</SettingsCallout>
    {/if}

    {#if library.dir !== ''}
      <p class="px-1 pt-1 text-[0.75rem] text-fg-hint">
        Or drop a WAV straight into
        <span class="select-all font-mono text-fg-muted">{library.dir}</span>. SOUNDS.md in that
        folder has the exact format, and any agent can convert a file for you.
      </p>
    {/if}

    {#if library.warnings.length > 0}
      <ul
        class="flex flex-col gap-0.5 px-1 text-[0.75rem] text-warning"
        data-testid="settings-sound-warnings"
      >
        {#each library.warnings as warning (warning)}
          <li>{warning}</li>
        {/each}
      </ul>
    {/if}

    {#if customSoundsError()}
      <SettingsCallout tone="warn">{customSoundsError()}</SettingsCallout>
    {/if}
  </div>
</div>

<ConfirmDialog
  open={pendingDelete !== null}
  title="Delete sound"
  description={pendingDelete === null ? '' : deleteDescription(pendingDelete)}
  confirmLabel="Delete"
  destructive={true}
  onConfirm={() => {
    const id = pendingDelete;
    pendingDelete = null;
    if (id !== null) void removeSound(id);
  }}
  onCancel={() => {
    pendingDelete = null;
  }}
/>
