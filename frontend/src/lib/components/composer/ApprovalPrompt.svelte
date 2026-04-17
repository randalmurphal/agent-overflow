<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { ApprovalRequest } from '../../types/events';
  import { RespondToApproval, ApprovalResponse, ElicitationResolution, PermissionProfile } from '../../stores/bindings';
  import { parseElicitationSchema, type ElicitationField } from '../../utils/elicitationSchema';

  let { pane }: { pane: ThreadPane } = $props();

  let containerEl: HTMLDivElement | undefined = $state(undefined);
  let previousFocus: Element | null = null;

  // Move focus into the alertdialog when approvals appear; restore on dismiss
  $effect(() => {
    if (pane.pendingApprovals.length > 0 && containerEl) {
      previousFocus = document.activeElement;
      const first = containerEl.querySelector<HTMLElement>('button, input, select, textarea');
      first?.focus();
    } else if (pane.pendingApprovals.length === 0 && previousFocus instanceof HTMLElement) {
      previousFocus.focus();
      previousFocus = null;
    }
  });

  // Per-approval answer state for user-input kind
  let answers: Map<string, Record<string, string>> = $state(new Map());
  // Per-approval permission scope for permission kind
  let permissionScopes: Map<string, 'turn' | 'session'> = $state(new Map());

  function getPermissionScope(requestId: string): 'turn' | 'session' {
    return permissionScopes.get(requestId) ?? 'turn';
  }

  function setPermissionScope(requestId: string, scope: 'turn' | 'session') {
    permissionScopes = new Map(permissionScopes).set(requestId, scope);
  }

  function getAnswer(requestId: string, questionId: string): string {
    return answers.get(requestId)?.[questionId] ?? '';
  }

  function setAnswer(requestId: string, questionId: string, value: string) {
    const current = answers.get(requestId) ?? {};
    answers = new Map(answers).set(requestId, { ...current, [questionId]: value });
  }

  async function handleApproval(requestId: string, decision: 'allow' | 'deny') {
    const threadId = pane.threadId;
    if (!threadId) return;

    try {
      await RespondToApproval(threadId, new ApprovalResponse({ requestId, decision }));
    } catch (err) {
      console.error('Failed to respond to approval:', err);
      pane.setError(`Failed to respond to approval: ${err}`);
    }
  }

  async function handleUserInputSubmit(approval: ApprovalRequest) {
    const threadId = pane.threadId;
    if (!threadId) return;

    const collected = answers.get(approval.requestId) ?? {};
    try {
      await RespondToApproval(threadId, new ApprovalResponse({
        requestId: approval.requestId,
        decision: 'allow',
        answers: collected,
      }));
    } catch (err) {
      console.error('Failed to submit user input:', err);
      pane.setError(`Failed to submit input: ${err}`);
    }
  }

  async function handlePermissionGrant(approval: ApprovalRequest) {
    const threadId = pane.threadId;
    if (!threadId) return;

    const scope = getPermissionScope(approval.requestId);
    try {
      // Cast needed: our local PermissionProfile interface uses optional fields,
      // while the Wails binding class uses required-but-nullable fields.
      const perms = new PermissionProfile(approval.permissions as Partial<InstanceType<typeof PermissionProfile>> ?? {});
      await RespondToApproval(threadId, new ApprovalResponse({
        requestId: approval.requestId,
        decision: 'allow',
        permissions: perms,
        scope,
      }));
    } catch (err) {
      console.error('Failed to grant permission:', err);
      pane.setError(`Failed to grant permission: ${err}`);
    }
  }

  async function handleAllowSession(approval: ApprovalRequest) {
    pane.addSessionApprovedTool(approval.toolName);
    await handleApproval(approval.requestId, 'allow');
  }

  // ---- MCP elicitation state + handlers ----

  // Per-request field values. Keyed by requestId → field name → value.
  // Values are typed by field kind: string, number, boolean, or string[].
  let elicitationValues: Map<string, Record<string, unknown>> = $state(new Map());
  // Per-request validation errors, same key structure.
  let elicitationErrors: Map<string, Record<string, string>> = $state(new Map());

  // Cache parsed fields per requestId so we don't re-parse on every render.
  // Schema arrives once per approval so a Map keyed by requestId is safe.
  function fieldsFor(approval: ApprovalRequest): ElicitationField[] {
    if (!approval.elicitation || approval.elicitation.mode !== 'form') return [];
    return parseElicitationSchema(approval.elicitation.requestedSchema);
  }

  function getElicitationValue(approval: ApprovalRequest, field: ElicitationField): unknown {
    const current = elicitationValues.get(approval.requestId);
    if (current && Object.prototype.hasOwnProperty.call(current, field.name)) {
      return current[field.name];
    }
    return field.default;
  }

  function setElicitationValue(requestId: string, name: string, value: unknown) {
    const next = { ...(elicitationValues.get(requestId) ?? {}), [name]: value };
    elicitationValues = new Map(elicitationValues).set(requestId, next);
    // Clear any stale error on this field — the user is trying to fix it.
    const errs = elicitationErrors.get(requestId);
    if (errs && errs[name]) {
      const cleared = { ...errs };
      delete cleared[name];
      elicitationErrors = new Map(elicitationErrors).set(requestId, cleared);
    }
  }

  function toggleMultiSelectValue(approval: ApprovalRequest, field: ElicitationField, value: string) {
    if (field.kind !== 'multi-select') return;
    const current = (getElicitationValue(approval, field) as string[] | undefined) ?? [];
    const next = current.includes(value)
      ? current.filter((v) => v !== value)
      : [...current, value];
    setElicitationValue(approval.requestId, field.name, next);
  }

  function getElicitationError(requestId: string, name: string): string | undefined {
    return elicitationErrors.get(requestId)?.[name];
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

  function validateElicitationForm(approval: ApprovalRequest, fields: ElicitationField[]): boolean {
    const errs: Record<string, string> = {};
    for (const field of fields) {
      const value = getElicitationValue(approval, field);
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
    elicitationErrors = new Map(elicitationErrors).set(approval.requestId, errs);
    return Object.keys(errs).length === 0;
  }

  function buildElicitationContent(approval: ApprovalRequest, fields: ElicitationField[]): Record<string, unknown> {
    const out: Record<string, unknown> = {};
    for (const field of fields) {
      const value = getElicitationValue(approval, field);
      if (value === undefined) continue;
      if (typeof value === 'string' && value.length === 0 && !field.required) continue;
      if (Array.isArray(value) && value.length === 0 && !field.required) continue;
      out[field.name] = value;
    }
    return out;
  }

  async function respondElicitation(
    approval: ApprovalRequest,
    action: 'accept' | 'decline' | 'cancel',
    content: Record<string, unknown> | null,
  ) {
    const threadId = pane.threadId;
    if (!threadId) return;
    try {
      await RespondToApproval(threadId, new ApprovalResponse({
        requestId: approval.requestId,
        decision: action,
        elicitation: new ElicitationResolution({
          action,
          content: content ?? undefined,
        }),
      }));
    } catch (err) {
      console.error('Failed to respond to elicitation:', err);
      pane.setError(`Failed to respond to elicitation: ${err}`);
    }
  }

  async function handleElicitationAccept(approval: ApprovalRequest) {
    if (approval.elicitation?.mode === 'url') {
      await respondElicitation(approval, 'accept', null);
      return;
    }
    const fields = fieldsFor(approval);
    if (!validateElicitationForm(approval, fields)) return;
    await respondElicitation(approval, 'accept', buildElicitationContent(approval, fields));
  }

  async function handleElicitationDecline(approval: ApprovalRequest) {
    await respondElicitation(approval, 'decline', null);
  }

  async function handleElicitationCancel(approval: ApprovalRequest) {
    await respondElicitation(approval, 'cancel', null);
  }

  function handleOpenElicitationURL(url: string) {
    // The system webview will hand this off to the OS browser. Using
    // noopener/noreferrer so the tab can't reach back into the app context.
    window.open(url, '_blank', 'noopener,noreferrer');
  }

  function isCommandTool(toolName: string): boolean {
    return toolName === 'Bash' || toolName === 'bash' ||
           toolName === 'execute_command' || toolName === 'shell';
  }

  function isFileTool(toolName: string): boolean {
    return toolName === 'Read' || toolName === 'Write' || toolName === 'Edit' ||
           toolName === 'read_file' || toolName === 'write_file' || toolName === 'edit_file';
  }

  function getInputPreview(approval: ApprovalRequest): { label: string; content: string } | null {
    if (!approval.input) return null;
    const input = approval.input as Record<string, unknown>;

    if (isCommandTool(approval.toolName)) {
      const cmd = input.command ?? input.cmd ?? '';
      return cmd ? { label: 'Command', content: String(cmd) } : null;
    }

    if (isFileTool(approval.toolName)) {
      const path = input.file_path ?? input.path ?? input.filePath ?? '';
      return path ? { label: 'File', content: String(path) } : null;
    }

    return null;
  }
</script>

{#if pane.pendingApprovals.length > 0}
  <div bind:this={containerEl} role="alertdialog" aria-live="assertive" aria-label="Tool approval required" class="border-t border-border bg-surface-1 px-4 py-3 space-y-2">
    {#each pane.pendingApprovals as approval (approval.requestId)}
      <div class="rounded border border-accent/40 bg-surface-0 px-3 py-2.5">
        <div class="flex items-start justify-between gap-3">
          <div class="min-w-0 flex-1">
            <p class="text-sm font-medium text-accent">{approval.toolName}</p>
            <p class="text-xs text-text-secondary mt-0.5">{approval.description || approval.title}</p>
          </div>
        </div>

        {#if approval.kind === 'user-input' && approval.questions?.length}
          <!-- User-input kind: render questions -->
          <div class="mt-2 space-y-2">
            {#each approval.questions as question (question.id)}
              <div>
                {#if question.header}
                  <p class="text-xs font-medium text-text-primary">{question.header}</p>
                {/if}
                <label class="text-xs text-text-secondary block mt-0.5" for="q-{approval.requestId}-{question.id}">{question.question}</label>
                {#if question.options?.length}
                  <select
                    id="q-{approval.requestId}-{question.id}"
                    class="mt-1 w-full text-xs rounded border border-border bg-surface-1 px-2 py-1.5 text-text-primary"
                    value={getAnswer(approval.requestId, question.id)}
                    onchange={(e) => setAnswer(approval.requestId, question.id, (e.target as HTMLSelectElement).value)}
                  >
                    <option value="">Select...</option>
                    {#each question.options as opt}
                      <option value={opt.label}>{opt.label}{opt.description ? ` — ${opt.description}` : ''}</option>
                    {/each}
                  </select>
                {:else}
                  <input
                    id="q-{approval.requestId}-{question.id}"
                    type="text"
                    class="mt-1 w-full text-xs rounded border border-border bg-surface-1 px-2 py-1.5 text-text-primary"
                    value={getAnswer(approval.requestId, question.id)}
                    oninput={(e) => setAnswer(approval.requestId, question.id, (e.target as HTMLInputElement).value)}
                    placeholder="Enter response..."
                  />
                {/if}
              </div>
            {/each}
          </div>
          <div class="flex gap-2 mt-2.5 justify-end">
            <button
              onclick={() => handleUserInputSubmit(approval)}
              class="px-3 py-1 text-xs rounded bg-accent text-surface-0 font-medium hover:opacity-90 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
            >
              Submit
            </button>
            <button
              onclick={() => handleApproval(approval.requestId, 'deny')}
              class="px-3 py-1 text-xs rounded border border-error/40 text-error hover:bg-error/10 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
            >
              Cancel
            </button>
          </div>

        {:else if approval.kind === 'permission' && approval.permissions}
          <!-- Permission kind: show permission details + scope -->
          <div class="mt-2 rounded bg-surface-1 border border-border px-2.5 py-1.5">
            <span class="text-[10px] text-text-secondary/60 block mb-1">Requested Permissions</span>
            {#if approval.permissions.network}
              <p class="text-xs text-text-primary">Network: {approval.permissions.network.enabled ? 'Enabled' : 'Disabled'}</p>
            {/if}
            {#if approval.permissions.fileSystem}
              {#if approval.permissions.fileSystem.read?.length}
                <p class="text-xs text-text-primary">Read: {approval.permissions.fileSystem.read.join(', ')}</p>
              {/if}
              {#if approval.permissions.fileSystem.write?.length}
                <p class="text-xs text-text-primary">Write: {approval.permissions.fileSystem.write.join(', ')}</p>
              {/if}
            {/if}
          </div>
          <div class="mt-2 flex items-center gap-3">
            <span class="text-xs text-text-secondary">Scope:</span>
            <label class="flex items-center gap-1 text-xs text-text-primary cursor-pointer">
              <input
                type="radio"
                name="scope-{approval.requestId}"
                value="turn"
                checked={getPermissionScope(approval.requestId) === 'turn'}
                onchange={() => setPermissionScope(approval.requestId, 'turn')}
              />
              This turn only
            </label>
            <label class="flex items-center gap-1 text-xs text-text-primary cursor-pointer">
              <input
                type="radio"
                name="scope-{approval.requestId}"
                value="session"
                checked={getPermissionScope(approval.requestId) === 'session'}
                onchange={() => setPermissionScope(approval.requestId, 'session')}
              />
              This session
            </label>
          </div>
          <div class="flex gap-2 mt-2.5 justify-end">
            <button
              onclick={() => handlePermissionGrant(approval)}
              class="px-3 py-1 text-xs rounded bg-accent text-surface-0 font-medium hover:opacity-90 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
            >
              Grant
            </button>
            <button
              onclick={() => handleApproval(approval.requestId, 'deny')}
              class="px-3 py-1 text-xs rounded border border-error/40 text-error hover:bg-error/10 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
            >
              Deny
            </button>
          </div>

        {:else if approval.kind === 'mcp-elicitation' && approval.elicitation}
          <!-- MCP elicitation: form-mode schema or URL-mode redirect -->
          {@const el = approval.elicitation}
          <div class="mt-2 space-y-2">
            {#if el.serverName}
              <p class="text-[10px] text-text-secondary/80" data-testid="elicitation-server">
                From MCP server: <span class="font-medium text-text-primary">{el.serverName}</span>
              </p>
            {/if}
            {#if el.message}
              <p class="text-xs text-text-secondary" data-testid="elicitation-message">{el.message}</p>
            {/if}

            {#if el.mode === 'url' && el.url}
              <div
                class="rounded border border-border bg-surface-1 px-2.5 py-2"
                data-testid="elicitation-url-panel"
              >
                <p class="text-[10px] text-text-secondary/70 mb-1">External approval URL</p>
                <a
                  href={el.url}
                  target="_blank"
                  rel="noopener noreferrer"
                  class="text-xs text-accent break-all hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
                  data-testid="elicitation-url-link"
                  onclick={(e) => { e.preventDefault(); handleOpenElicitationURL(el.url!); }}
                >
                  {el.url}
                </a>
                <p class="text-[10px] text-text-secondary/70 mt-1">
                  Click "Accept" after completing the flow in your browser.
                </p>
              </div>
            {:else}
              {@const fields = fieldsFor(approval)}
              {#if fields.length === 0}
                <p class="text-xs italic text-text-secondary" data-testid="elicitation-empty-schema">
                  The server did not send any fields to collect.
                </p>
              {:else}
                <div class="space-y-2" data-testid="elicitation-fields">
                  {#each fields as field (field.name)}
                    <div>
                      <label for="el-{approval.requestId}-{field.name}" class="text-xs font-medium text-text-primary">
                        {field.title}
                        {#if field.required}<span aria-label="required" class="text-error">*</span>{/if}
                      </label>
                      {#if field.description}
                        <p class="text-[10px] text-text-secondary/80">{field.description}</p>
                      {/if}

                      {#if field.kind === 'string'}
                        <input
                          id="el-{approval.requestId}-{field.name}"
                          type={field.format === 'email' ? 'email' : field.format === 'uri' ? 'url' : field.format === 'date' ? 'date' : field.format === 'date-time' ? 'datetime-local' : 'text'}
                          value={(getElicitationValue(approval, field) as string) ?? ''}
                          minlength={field.minLength}
                          maxlength={field.maxLength}
                          data-testid="el-input-{field.name}"
                          class="mt-1 w-full text-xs rounded border border-border bg-surface-1 px-2 py-1.5 text-text-primary"
                          oninput={(e) => setElicitationValue(approval.requestId, field.name, (e.target as HTMLInputElement).value)}
                        />
                      {:else if field.kind === 'number'}
                        <input
                          id="el-{approval.requestId}-{field.name}"
                          type="number"
                          value={(getElicitationValue(approval, field) as number | undefined) ?? ''}
                          step={field.integer ? 1 : 'any'}
                          min={field.minimum}
                          max={field.maximum}
                          data-testid="el-input-{field.name}"
                          class="mt-1 w-full text-xs rounded border border-border bg-surface-1 px-2 py-1.5 text-text-primary"
                          oninput={(e) => {
                            const raw = (e.target as HTMLInputElement).value;
                            if (raw === '') {
                              setElicitationValue(approval.requestId, field.name, undefined);
                              return;
                            }
                            const n = field.integer ? parseInt(raw, 10) : parseFloat(raw);
                            setElicitationValue(approval.requestId, field.name, Number.isFinite(n) ? n : undefined);
                          }}
                        />
                      {:else if field.kind === 'boolean'}
                        <div class="mt-1 flex items-center gap-2">
                          <input
                            id="el-{approval.requestId}-{field.name}"
                            type="checkbox"
                            checked={(getElicitationValue(approval, field) as boolean | undefined) === true}
                            data-testid="el-input-{field.name}"
                            onchange={(e) => setElicitationValue(approval.requestId, field.name, (e.target as HTMLInputElement).checked)}
                          />
                          <label for="el-{approval.requestId}-{field.name}" class="text-xs text-text-primary">
                            Enabled
                          </label>
                        </div>
                      {:else if field.kind === 'select'}
                        <select
                          id="el-{approval.requestId}-{field.name}"
                          value={(getElicitationValue(approval, field) as string | undefined) ?? ''}
                          data-testid="el-input-{field.name}"
                          class="mt-1 w-full text-xs rounded border border-border bg-surface-1 px-2 py-1.5 text-text-primary"
                          onchange={(e) => setElicitationValue(approval.requestId, field.name, (e.target as HTMLSelectElement).value)}
                        >
                          <option value="">Select…</option>
                          {#each field.options as opt (opt.value)}
                            <option value={opt.value}>{opt.label}</option>
                          {/each}
                        </select>
                      {:else if field.kind === 'multi-select'}
                        <div class="mt-1 space-y-1" data-testid="el-input-{field.name}">
                          {#each field.options as opt (opt.value)}
                            {@const selected = ((getElicitationValue(approval, field) as string[] | undefined) ?? []).includes(opt.value)}
                            <label class="flex items-center gap-2 text-xs text-text-primary">
                              <input
                                type="checkbox"
                                checked={selected}
                                data-testid="el-option-{field.name}-{opt.value}"
                                onchange={() => toggleMultiSelectValue(approval, field, opt.value)}
                              />
                              {opt.label}
                            </label>
                          {/each}
                        </div>
                      {/if}

                      {#if getElicitationError(approval.requestId, field.name)}
                        <p class="mt-1 text-[10px] text-error" data-testid="el-error-{field.name}">
                          {getElicitationError(approval.requestId, field.name)}
                        </p>
                      {/if}
                    </div>
                  {/each}
                </div>
              {/if}
            {/if}
          </div>
          <div class="flex flex-wrap gap-2 mt-2.5 justify-end">
            <button
              type="button"
              data-testid="elicitation-cancel"
              onclick={() => handleElicitationCancel(approval)}
              class="px-3 py-1 text-xs rounded border border-border text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
            >
              Cancel
            </button>
            <button
              type="button"
              data-testid="elicitation-decline"
              onclick={() => handleElicitationDecline(approval)}
              class="px-3 py-1 text-xs rounded border border-error/40 text-error hover:bg-error/10 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
            >
              Decline
            </button>
            <button
              type="button"
              data-testid="elicitation-accept"
              onclick={() => handleElicitationAccept(approval)}
              class="px-3 py-1 text-xs rounded bg-accent text-surface-0 font-medium hover:opacity-90 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
            >
              Accept
            </button>
          </div>

        {:else}
          <!-- Default tool approval: allow / allow-for-session / deny -->
          {@const preview = getInputPreview(approval)}
          {#if preview}
            <div class="mt-2 rounded bg-surface-1 border border-border px-2.5 py-1.5 max-h-40 overflow-y-auto">
              <span class="text-[10px] text-text-secondary/60 block mb-0.5">{preview.label}</span>
              <pre class="text-xs font-mono text-text-primary whitespace-pre-wrap">{preview.content}</pre>
            </div>
          {/if}

          <div class="flex flex-wrap gap-2 mt-2.5 justify-end">
            <button
              onclick={() => handleApproval(approval.requestId, 'allow')}
              class="px-3 py-1 text-xs rounded bg-accent text-surface-0 font-medium hover:bg-accent/85 cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
            >
              Allow
            </button>
            <button
              onclick={() => handleAllowSession(approval)}
              class="px-3 py-1 text-xs rounded border border-accent/40 text-accent hover:bg-accent/10 cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
            >
              Allow for Session
            </button>
            <button
              onclick={() => handleApproval(approval.requestId, 'deny')}
              class="px-3 py-1 text-xs rounded border border-error/40 text-error hover:bg-error/10 cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
            >
              Deny
            </button>
          </div>
        {/if}
      </div>
    {/each}
  </div>
{/if}
