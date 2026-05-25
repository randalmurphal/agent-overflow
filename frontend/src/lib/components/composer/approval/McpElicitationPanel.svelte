<script lang="ts">
  import { onMount } from 'svelte';
  import type { ApprovalRequest } from '../../../types/events';
  import { ApprovalResponse, ElicitationResolution } from '../../../stores/bindings';
  import { parseElicitationSchema, type ElicitationField } from '../../../utils/elicitationSchema';
  import { errString } from '../../../utils/errors';
  import { handleExternalURL, safeExternalURL } from '../../../utils/externalLinks';
  import McpElicitationField from './McpElicitationField.svelte';
  import Button from '../../primitives/Button.svelte';
  import {
    composerRootFor,
    composerTextareaHasFocus,
  } from '../composerFocus';
  import {
    focusApprovalActionFromKey,
    focusApprovalActionContainer,
  } from './approvalActionKeyboard';

  interface Props {
    approval: ApprovalRequest;
    onResolve: (response: ApprovalResponse) => Promise<void>;
    onError: (message: string) => void;
    responding?: boolean;
  }

  let { approval, onResolve, onError, responding = false }: Props = $props();
  let actionRow: HTMLDivElement | undefined = $state(undefined);

  // Schema arrives once per approval — parse once up front and reuse.
  const fields: ElicitationField[] = $derived(
    approval.elicitation?.mode === 'form'
      ? parseElicitationSchema(approval.elicitation.requestedSchema)
      : [],
  );

  // Field values keyed by field name. Typed as unknown because values can be
  // string, number, boolean, or string[] depending on field.kind.
  function createFieldMap<T>(): Record<string, T> {
    return Object.create(null) as Record<string, T>;
  }

  let values: Record<string, unknown> = $state(createFieldMap<unknown>());
  let errors: Record<string, string> = $state(createFieldMap<string>());

  function getValue(field: ElicitationField): unknown {
    if (Object.prototype.hasOwnProperty.call(values, field.name)) {
      return values[field.name];
    }
    return field.default;
  }

  function setValue(name: string, value: unknown) {
    values = Object.assign(createFieldMap<unknown>(), values, { [name]: value });
    // Clear any stale error on this field — the user is trying to fix it.
    if (errors[name]) {
      const cleared = Object.assign(createFieldMap<string>(), errors);
      delete cleared[name];
      errors = cleared;
    }
  }

  function toggleMultiSelectValue(field: ElicitationField, value: string) {
    if (field.kind !== 'multi-select') return;
    const current = (getValue(field) as string[] | undefined) ?? [];
    const next = current.includes(value)
      ? current.filter((v) => v !== value)
      : [...current, value];
    setValue(field.name, next);
  }

  // Simple format validators — match the MCP-allowed `format` hints.
  function validateFormatHint(value: string, hint: string | undefined): string | null {
    if (!hint || value.length === 0) return null;
    if (hint === 'email') {
      // Narrow: one @, one dot in the domain, no whitespace. Good enough for
      // a form hint; server-side still validates.
      return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(value) ? null : 'Not a valid email address.';
    }
    if (hint === 'uri') {
      try {
        // eslint-disable-next-line no-new
        new URL(value);
        return null;
      } catch {
        return 'Not a valid URL.';
      }
    }
    if (hint === 'date') {
      return /^\d{4}-\d{2}-\d{2}$/.test(value) ? null : 'Use YYYY-MM-DD.';
    }
    if (hint === 'date-time') {
      // RFC3339-lite. Accept the subset the UI emits via <input type="datetime-local">.
      return /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?(\.\d+)?(Z|[+-]\d{2}:\d{2})?$/.test(value)
        ? null
        : 'Use an ISO 8601 date-time.';
    }
    return null;
  }

  function validateForm(): boolean {
    const errs: Record<string, string> = createFieldMap<string>();
    for (const field of fields) {
      const value = getValue(field);
      const empty =
        value === undefined
        || value === null
        || (typeof value === 'string' && value.length === 0)
        || (Array.isArray(value) && value.length === 0);

      if (field.required && empty) {
        errs[field.name] = 'This field is required.';
        continue;
      }
      if (empty) continue; // Optional fields may be left blank.

      if (field.kind === 'string' && typeof value === 'string') {
        if (field.minLength !== undefined && value.length < field.minLength) {
          errs[field.name] = `Minimum ${field.minLength} characters.`;
          continue;
        }
        if (field.maxLength !== undefined && value.length > field.maxLength) {
          errs[field.name] = `Maximum ${field.maxLength} characters.`;
          continue;
        }
        const fmtErr = validateFormatHint(value, field.format);
        if (fmtErr) {
          errs[field.name] = fmtErr;
          continue;
        }
      }

      if (field.kind === 'number' && typeof value === 'number' && Number.isFinite(value)) {
        if (field.integer && !Number.isInteger(value)) {
          errs[field.name] = 'Must be a whole number.';
          continue;
        }
        if (field.minimum !== undefined && value < field.minimum) {
          errs[field.name] = `Must be ≥ ${field.minimum}.`;
          continue;
        }
        if (field.maximum !== undefined && value > field.maximum) {
          errs[field.name] = `Must be ≤ ${field.maximum}.`;
          continue;
        }
      }

      if (field.kind === 'multi-select' && Array.isArray(value)) {
        if (field.minItems !== undefined && value.length < field.minItems) {
          errs[field.name] = `Pick at least ${field.minItems}.`;
          continue;
        }
        if (field.maxItems !== undefined && value.length > field.maxItems) {
          errs[field.name] = `Pick at most ${field.maxItems}.`;
          continue;
        }
      }
    }
    errors = errs;
    return Object.keys(errs).length === 0;
  }

  function buildContent(): Record<string, unknown> {
    const out: Record<string, unknown> = createFieldMap<unknown>();
    for (const field of fields) {
      const value = getValue(field);
      if (value === undefined) continue;
      if (typeof value === 'string' && value.length === 0 && !field.required) continue;
      if (Array.isArray(value) && value.length === 0 && !field.required) continue;
      out[field.name] = value;
    }
    return out;
  }

  async function respond(
    action: 'accept' | 'decline' | 'cancel',
    content: Record<string, unknown> | null,
  ) {
    try {
      await onResolve(new ApprovalResponse({
        requestId: approval.requestId,
        decision: action,
        elicitation: new ElicitationResolution({
          action,
          content: content ?? undefined,
        }),
      }));
    } catch (err) {
      console.error('Failed to respond to elicitation:', err);
      onError(`Failed to respond to elicitation: ${errString(err)}`);
    }
  }

  async function accept() {
    if (approval.elicitation?.mode === 'url') {
      await respond('accept', null);
      return;
    }
    if (!validateForm()) return;
    await respond('accept', buildContent());
  }

  async function decline() {
    await respond('decline', null);
  }

  async function cancel() {
    await respond('cancel', null);
  }

  function handleOpenURL(url: string) {
    const safeURL = safeExternalURL(url);
    if (!safeURL) {
      onError('MCP server provided an unsupported approval URL.');
      return;
    }
    void handleExternalURL(safeURL);
  }

  onMount(() => {
    if (composerTextareaHasFocus(composerRootFor(actionRow))) {
      queueMicrotask(() => focusApprovalActionContainer(actionRow));
    }
  });
</script>

{#if approval.elicitation}
  {@const el = approval.elicitation}
  <div class="mt-2 space-y-2">
    {#if el.serverName}
      <p class="text-[0.625rem] text-text-secondary/80" data-testid="elicitation-server">
        From MCP server: <span class="font-medium text-text-primary">{el.serverName}</span>
      </p>
    {/if}
    {#if el.message}
      <p class="text-xs text-text-secondary" data-testid="elicitation-message">{el.message}</p>
    {/if}

    {#if el.mode === 'url' && el.url}
      {@const safeURL = safeExternalURL(el.url)}
      <div
        class="rounded border border-border bg-surface-1 px-2.5 py-2"
        data-testid="elicitation-url-panel"
      >
        <p class="text-[0.625rem] text-text-secondary/70 mb-1">External approval URL</p>
        {#if safeURL}
          <a
            href={safeURL}
            class="text-xs text-accent break-all hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
            data-testid="elicitation-url-link"
            onclick={(e) => { e.preventDefault(); handleOpenURL(safeURL); }}
          >
            {safeURL}
          </a>
          <p class="text-[0.625rem] text-text-secondary/70 mt-1">
            Click "Accept" after completing the flow in your browser.
          </p>
        {:else}
          <p class="break-all text-xs text-error/90" data-testid="elicitation-url-blocked">
            Unsupported approval URL.
          </p>
        {/if}
      </div>
    {:else if fields.length === 0}
      <p class="text-xs italic text-text-secondary" data-testid="elicitation-empty-schema">
        The server did not send any fields to collect.
      </p>
    {:else}
      <div class="space-y-2" data-testid="elicitation-fields">
        {#each fields as field (field.name)}
          <McpElicitationField
            requestId={approval.requestId}
            {field}
            value={getValue(field)}
            error={errors[field.name]}
            onChange={setValue}
            onToggleOption={toggleMultiSelectValue}
          />
        {/each}
      </div>
    {/if}
  </div>

  <div
    bind:this={actionRow}
    class="flex flex-wrap gap-2 mt-2.5 justify-end"
    role="toolbar"
    aria-label="MCP approval actions"
    tabindex="0"
    onkeydown={(event) => focusApprovalActionFromKey(event, actionRow)}
  >
    <Button variant="secondary" size="sm" onclick={cancel} testId="elicitation-cancel" disabled={responding}>
      {#snippet children()}Cancel{/snippet}
    </Button>
    <Button variant="danger-outline" size="sm" onclick={decline} testId="elicitation-decline" disabled={responding}>
      {#snippet children()}Decline{/snippet}
    </Button>
    <Button variant="primary" size="sm" onclick={accept} testId="elicitation-accept" loading={responding}>
      {#snippet children()}Accept{/snippet}
    </Button>
  </div>
{/if}
