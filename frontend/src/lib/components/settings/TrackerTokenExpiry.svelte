<script lang="ts">
  // The token's expiry as the person read it on the tracker's token page: a
  // date, kept as the start of that day here (so the warning never comes
  // late), or none.
  import { INPUT_CLASS, SECONDARY_BUTTON_CLASS, GHOST_BUTTON_CLASS } from './styles';
  import { localDateValue, startOfLocalDay } from './trackerConnectionStatus';

  let {
    expiresAt,
    disabled = false,
    disabledReason,
    onSave,
    onCancel,
  }: {
    expiresAt: number;
    disabled?: boolean;
    disabledReason?: string;
    /** Unix milliseconds; 0 clears. */
    onSave: (expiresAt: number) => Promise<boolean>;
    onCancel?: () => void;
  } = $props();

  const id = $props.id();
  // A draft of the date: it starts from the recorded one and is saved only
  // by Save.
  // svelte-ignore state_referenced_locally
  let value = $state(expiresAt > 0 ? localDateValue(expiresAt) : '');
  let saved = $derived(expiresAt > 0 ? localDateValue(expiresAt) : '');

  async function save(): Promise<void> {
    if (await onSave(startOfLocalDay(value))) onCancel?.();
  }

  async function clear(): Promise<void> {
    if (await onSave(0)) {
      value = '';
      onCancel?.();
    }
  }
</script>

<div class="flex flex-col gap-1.5" data-testid="tracker-token-expiry">
  <label for="{id}-date" class="text-[0.75rem] font-medium text-fg">When does the token expire?</label>
  <p class="text-[0.71875rem] text-fg-muted">
    Optional. The tracker shows the date when it makes the token; the row warns two weeks before.
  </p>
  <div class="flex flex-wrap items-center gap-2">
    <input
      id="{id}-date"
      type="date"
      class="{INPUT_CLASS} max-w-[12rem]"
      bind:value
      {disabled}
      title={disabledReason}
      data-testid="tracker-token-expiry-date"
    />
    <button
      type="button"
      class={SECONDARY_BUTTON_CLASS}
      disabled={disabled || value === '' || value === saved}
      title={disabledReason}
      onclick={save}
      data-testid="tracker-token-expiry-save"
    >Save date</button>
    {#if expiresAt > 0}
      <button
        type="button"
        class={GHOST_BUTTON_CLASS}
        {disabled}
        title={disabledReason}
        onclick={clear}
        data-testid="tracker-token-expiry-clear"
      >Clear</button>
    {/if}
    {#if onCancel}
      <button type="button" class={GHOST_BUTTON_CLASS} onclick={onCancel}>Cancel</button>
    {/if}
  </div>
</div>
