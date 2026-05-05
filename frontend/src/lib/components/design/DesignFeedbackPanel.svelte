<script lang="ts">
  // DesignFeedbackPanel — UI-side accumulator for the user's feedback
  // before it's sent to the agent.
  //
  // Three input affordances:
  //   1. Sliders bound to `pane.exposedControls`. The agent emits these
  //      via an `expose_controls` aoflow-design block; the parser in
  //      `frontend/src/lib/utils/designAssistantPayload.ts` projects
  //      them onto pane state when the assistant message lands.
  //   2. A free-form notes textarea.
  //   3. A comments accumulator (currently the same notes textarea —
  //      element-targeted comments via selector capture are a future
  //      addition).
  //
  // On "Send to agent" the staged inputs are serialised into a fenced
  // `aoflow-design` JSON block that the agent's prompt instructions
  // teach it to read. The block is the body of a regular SendMessage
  // call — no parallel wire format. Sliders that haven't moved are
  // omitted; an empty batch is rejected by `canSend`.

  import { untrack } from 'svelte';
  import Send from 'lucide-svelte/icons/send';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { FeedbackBatch, SliderChange } from '../../types/design';
  import { SendMessage } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import Icon from '../primitives/Icon.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  // Local user-facing slider values keyed by slider id. We intentionally
  // do NOT mirror the slider state into pane storage — once the agent
  // emits a new `exposedControls` set, the previous user-staged values
  // are abandoned along with the previous knob set. That's the
  // wholesale-replacement contract from the wire types.
  let sliderValues = $state<Record<string, number>>({});
  let notes = $state('');
  let sending = $state(false);

  // When the agent publishes a new control set, seed the local slider
  // map with the agent-supplied initial values for every knob the user
  // hasn't already touched. Touched knobs persist across re-emissions
  // with the same id, mirroring "the user is mid-tweak — don't yank
  // their slider."
  //
  // The reads of `sliderValues` happen inside `untrack` so the effect
  // does NOT subscribe to the very state it writes — without that, the
  // assignment `sliderValues = next` re-triggers the effect (Svelte 5
  // invalidates on object identity even when the contents match), the
  // effect builds another fresh map and writes again, and the whole
  // tab wedges in a synchronous reactive loop. The first time the
  // agent published an `expose_controls` set the loop tripped and
  // jammed the main thread, which also starved the screenshot
  // postMessage round-trip and surfaced as a `read_screenshot` timeout
  // error on the agent's tool call.
  $effect(() => {
    const controls = pane.exposedControls;
    untrack(() => {
      const next: Record<string, number> = {};
      for (const c of controls) {
        next[c.id] = sliderValues[c.id] ?? c.value;
      }
      // Drop any orphaned values whose slider was retracted.
      sliderValues = next;
    });
  });

  // sliderChanges only emits knobs whose user-set value differs from
  // the agent-published value. Sending a change that matches the agent's
  // value would be noise.
  let sliderChanges = $derived.by<SliderChange[]>(() => {
    const out: SliderChange[] = [];
    for (const c of pane.exposedControls) {
      const v = sliderValues[c.id];
      if (typeof v === 'number' && v !== c.value) {
        out.push({ id: c.id, value: v });
      }
    }
    return out;
  });

  let pendingChangeCount = $derived(
    sliderChanges.length + (notes.trim().length > 0 ? 1 : 0),
  );
  let canSend = $derived(pendingChangeCount > 0 && !sending && !!pane.threadId);

  function buildBatch(): FeedbackBatch {
    const batch: FeedbackBatch = {};
    if (sliderChanges.length > 0) batch.sliderChanges = sliderChanges;
    const trimmedNotes = notes.trim();
    if (trimmedNotes.length > 0) batch.notes = trimmedNotes;
    return batch;
  }

  function serialise(batch: FeedbackBatch): string {
    // Wrap the JSON body in the agent's expected fenced block. The
    // surrounding "User feedback for this turn:" line keeps the
    // context human-readable in the timeline; the fenced block is
    // what the agent's instructions tell it to parse.
    const json = JSON.stringify({ kind: 'feedback_batch', ...batch }, null, 2);
    return `User feedback for this turn:\n\n\`\`\`aoflow-design\n${json}\n\`\`\``;
  }

  async function send(): Promise<void> {
    if (!canSend) return;
    const threadId = pane.threadId;
    if (!threadId) return;
    const batch = buildBatch();
    sending = true;
    try {
      await SendMessage(threadId, serialise(batch), []);
      addToast('success', 'Feedback sent to agent');
      // Reset after successful send. Sliders re-seed from the agent's
      // current `value` field via the effect above; clearing the
      // sliderValues map empties the changes set without yanking the
      // visible slider position (the seed effect runs synchronously on
      // the next reactive flush).
      sliderValues = {};
      notes = '';
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      addToast('error', `Failed to send feedback: ${message}`);
    } finally {
      sending = false;
    }
  }
</script>

<section
  class="flex flex-col min-h-0 bg-transparent border-t border-border-subtle"
  data-testid="design-feedback-panel"
>
  <div class="flex items-center justify-between px-3 pt-3 pb-2 border-b border-border-subtle shrink-0">
    <p class="text-[11px] font-semibold uppercase tracking-[0.18em] text-fg-subtle">
      Feedback
    </p>
  </div>

  <div class="flex-1 min-h-0 overflow-y-auto px-3 py-2 space-y-3">
    {#if pane.exposedControls.length === 0}
      <p class="text-[11px] text-fg-hint">
        The agent will expose tweakable knobs here after a design iteration lands.
      </p>
    {:else}
      <div class="space-y-2">
        {#each pane.exposedControls as control (control.id)}
          {@const value = sliderValues[control.id] ?? control.value}
          <label
            class="flex flex-col gap-1 text-[12px] text-fg"
            data-testid="design-feedback-slider"
          >
            <span class="flex items-baseline justify-between gap-2">
              <span class="truncate">{control.label}</span>
              <span class="text-[10px] tabular-nums text-fg-muted">{value.toFixed(2)}</span>
            </span>
            <input
              type="range"
              min={control.min}
              max={control.max}
              step={control.step ?? 0.01}
              value={value}
              oninput={(e) => {
                const next = Number((e.currentTarget as HTMLInputElement).value);
                if (Number.isFinite(next)) {
                  sliderValues = { ...sliderValues, [control.id]: next };
                }
              }}
              class="w-full accent-accent"
              aria-label={control.label}
            />
          </label>
        {/each}
      </div>
    {/if}

    <label class="flex flex-col gap-1 text-[12px] text-fg">
      <span class="text-[11px] font-medium uppercase tracking-wide text-fg-subtle">
        Notes
      </span>
      <textarea
        bind:value={notes}
        rows="3"
        placeholder="Free-form feedback for the agent…"
        class={[
          'w-full rounded-[var(--radius-field)] border border-border-subtle',
          'bg-surface-0 px-2 py-1.5 text-[12px] text-fg',
          'placeholder:text-fg-hint resize-y min-h-[64px]',
          'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
        ].join(' ')}
        data-testid="design-feedback-notes"
      ></textarea>
    </label>
  </div>

  <div class="flex items-center justify-end gap-2 border-t border-border-subtle px-3 py-2 shrink-0">
    <span class="text-[10px] text-fg-hint tabular-nums">
      {pendingChangeCount}
      {pendingChangeCount === 1 ? 'change' : 'changes'}
    </span>
    <button
      type="button"
      onclick={() => void send()}
      disabled={!canSend}
      class={[
        'inline-flex items-center gap-1 rounded-[var(--radius-field)]',
        'border border-accent/60 bg-accent/15 px-3 py-1',
        'text-[12px] text-fg cursor-pointer transition-colors',
        'hover:bg-accent/25',
        'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
        'disabled:opacity-50 disabled:cursor-not-allowed',
      ].join(' ')}
      data-testid="design-feedback-send"
    >
      <Icon icon={Send} size={12} strokeWidth={1.7} class="shrink-0" />
      <span>{sending ? 'Sending…' : 'Send to agent'}</span>
    </button>
  </div>
</section>
