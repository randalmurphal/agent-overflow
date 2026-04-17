<script lang="ts">
  import type { DiscussionParticipant } from '../../types/discussion';

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
  class="rounded-2xl border border-border/60 bg-surface-0/55 p-4 shadow-[0_10px_40px_-24px_rgba(0,0,0,0.45)]"
  aria-label="Participant {index + 1}"
>
  <div class="mb-3 flex items-center justify-between gap-2">
    <legend class="text-[11px] font-semibold uppercase tracking-[0.22em] text-text-secondary/70">
      Participant {index + 1}
    </legend>
    <button
      type="button"
      onclick={onRemove}
      disabled={!canRemove}
      class="text-[11px] text-error/80 hover:text-error rounded-md px-2 py-1 cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-error/50"
      aria-label="Remove participant {index + 1}"
      title={canRemove ? 'Remove this participant' : 'At least two participants are required'}
    >
      Remove
    </button>
  </div>

  <div class="grid grid-cols-1 gap-3 md:grid-cols-2">
    <div>
      <label for={roleId} class="block text-xs font-medium text-text-primary mb-1">
        Role <span class="text-error/80">*</span>
      </label>
      <input
        id={roleId}
        type="text"
        value={participant.role}
        oninput={(e) => update('role', (e.target as HTMLInputElement).value)}
        placeholder="e.g. advocate, critic"
        class="w-full text-sm rounded-xl border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/50 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors"
      />
    </div>

    <div>
      <label for={descId} class="block text-xs font-medium text-text-primary mb-1">Description</label>
      <input
        id={descId}
        type="text"
        value={participant.description}
        oninput={(e) => update('description', (e.target as HTMLInputElement).value)}
        placeholder="How this role behaves"
        class="w-full text-sm rounded-xl border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/50 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors"
      />
    </div>

    <div>
      <label for={providerId} class="block text-xs font-medium text-text-primary mb-1">Provider</label>
      <select
        id={providerId}
        value={participant.provider ?? ''}
        onchange={(e) => updateProvider((e.target as HTMLSelectElement).value)}
        class="w-full text-sm rounded-xl border border-border bg-surface-0 px-3 py-2 text-text-primary focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors cursor-pointer"
      >
        <option value="">Inherit from parent thread</option>
        <option value="claude">Claude</option>
        <option value="codex">Codex</option>
      </select>
    </div>

    <div>
      <label for={modelId} class="block text-xs font-medium text-text-primary mb-1">Model</label>
      <input
        id={modelId}
        type="text"
        value={participant.model ?? ''}
        oninput={(e) => update('model', (e.target as HTMLInputElement).value)}
        placeholder="Inherit from parent thread"
        class="w-full text-sm rounded-xl border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/50 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors"
      />
    </div>
  </div>

  <div class="mt-3">
    <label for={systemId} class="block text-xs font-medium text-text-primary mb-1">
      System prompt <span class="text-error/80">*</span>
    </label>
    <textarea
      id={systemId}
      value={participant.system}
      oninput={(e) => update('system', (e.target as HTMLTextAreaElement).value)}
      rows={4}
      placeholder="What this participant believes, argues, prioritizes..."
      class="w-full text-sm rounded-xl border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/50 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors resize-y font-mono"
    ></textarea>
  </div>
</fieldset>
