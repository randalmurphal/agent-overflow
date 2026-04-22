<script lang="ts">
  import type { DiscussionParticipant } from '../../types/discussion';
  import Button from '../primitives/Button.svelte';

  let {
    participant,
    index,
    canRemove,
    onChange,
    onRemove,
  }: {
    participant: DiscussionParticipant;
    index: number;
    canRemove: boolean;
    onChange: (next: DiscussionParticipant) => void;
    onRemove: () => void;
  } = $props();

  function update<K extends keyof DiscussionParticipant>(key: K, value: DiscussionParticipant[K]): void {
    onChange({ ...participant, [key]: value });
  }

  function updateProvider(value: string): void {
    // Blank means "inherit from parent thread".
    onChange({ ...participant, provider: value });
  }

  let roleId = $derived(`participant-${index}-role`);
  let descId = $derived(`participant-${index}-desc`);
  let systemId = $derived(`participant-${index}-system`);
  let providerId = $derived(`participant-${index}-provider`);
  let modelId = $derived(`participant-${index}-model`);
</script>

<fieldset
  class="rounded-[var(--radius-card)] border border-border-subtle bg-card/30 p-4"
  aria-label="Participant {index + 1}"
>
  <div class="mb-3 flex items-center justify-between gap-2">
    <legend class="text-[11px] font-semibold uppercase tracking-[0.18em] text-fg-subtle">
      Participant {index + 1}
    </legend>
    <Button
      variant="danger-ghost"
      size="xs"
      onclick={onRemove}
      disabled={!canRemove}
      ariaLabel="Remove participant {index + 1}"
      title={canRemove ? 'Remove this participant' : 'At least two participants are required'}
    >
      {#snippet children()}Remove{/snippet}
    </Button>
  </div>

  <div class="grid grid-cols-1 gap-3 md:grid-cols-2">
    <div>
      <label for={roleId} class="block text-[12px] font-medium text-fg mb-1">
        Role <span class="text-error/80">*</span>
      </label>
      <input
        id={roleId}
        type="text"
        value={participant.role}
        oninput={(e) => update('role', (e.target as HTMLInputElement).value)}
        placeholder="e.g. advocate, critic"
        class="w-full text-[13px] rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-3 py-1.5 text-fg placeholder:text-fg-hint focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors"
      />
    </div>

    <div>
      <label for={descId} class="block text-[12px] font-medium text-fg mb-1">Description</label>
      <input
        id={descId}
        type="text"
        value={participant.description}
        oninput={(e) => update('description', (e.target as HTMLInputElement).value)}
        placeholder="How this role behaves"
        class="w-full text-[13px] rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-3 py-1.5 text-fg placeholder:text-fg-hint focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors"
      />
    </div>

    <div>
      <label for={providerId} class="block text-[12px] font-medium text-fg mb-1">Provider</label>
      <select
        id={providerId}
        value={participant.provider ?? ''}
        onchange={(e) => updateProvider((e.target as HTMLSelectElement).value)}
        class="w-full text-[13px] rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-3 py-1.5 text-fg focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors cursor-pointer"
      >
        <option value="">Inherit from parent thread</option>
        <option value="claude">Claude</option>
        <option value="codex">Codex</option>
      </select>
    </div>

    <div>
      <label for={modelId} class="block text-[12px] font-medium text-fg mb-1">Model</label>
      <input
        id={modelId}
        type="text"
        value={participant.model ?? ''}
        oninput={(e) => update('model', (e.target as HTMLInputElement).value)}
        placeholder="Inherit from parent thread"
        class="w-full text-[13px] rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-3 py-1.5 text-fg placeholder:text-fg-hint focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors"
      />
    </div>
  </div>

  <div class="mt-3">
    <label for={systemId} class="block text-[12px] font-medium text-fg mb-1">
      System prompt <span class="text-error/80">*</span>
    </label>
    <textarea
      id={systemId}
      value={participant.system}
      oninput={(e) => update('system', (e.target as HTMLTextAreaElement).value)}
      rows={4}
      placeholder="What this participant believes, argues, prioritizes…"
      class="w-full text-[13px] rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-3 py-2 text-fg placeholder:text-fg-hint focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors resize-y font-mono"
    ></textarea>
  </div>
</fieldset>
