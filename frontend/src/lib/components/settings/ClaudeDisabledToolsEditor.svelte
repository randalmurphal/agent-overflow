<script lang="ts">
  // Claude's disabled-tool list: free-form names, because Claude takes them
  // on `--disallowedTools` verbatim and its tool set changes release to
  // release. The suggestion row is a convenience over the common built-ins,
  // never a closed set — a name AO has never heard of is a legitimate entry,
  // and one the CLI has never heard of is simply ignored.
  //
  // The todo tools (CLAUDE_TODO_TOOL_GROUP) are the one exception to the
  // flat-chips rendering: they only make sense as a set, so they get one
  // grouped switch (with a per-tool disclosure for partial sets) instead of
  // five chips. Storage is still the same flat list — the group is a UI
  // projection, and a member typed into the free-form field lands in the
  // group's rows rather than as a stray chip.

  import { settingsComputer } from './settingsComputer';
  const { getSettings, updateSettingsPatch } = settingsComputer();
  import {
    CLAUDE_TODO_TOOL_GROUP,
    CLAUDE_TOOL_SUGGESTIONS,
    disabledToolNameError,
    disabledToolsFor,
    disabledToolsSettingsKey,
    exposedTodoTools,
    isTodoGroupTool,
    normalizeToolName,
    withToolAdded,
    withToolRemoved,
    withToolsAdded,
    withToolsRemoved,
  } from '../../utils/promptOverrides';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import type { ProviderDefinition } from '../../providers/catalog';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsField from './SettingsField.svelte';
  import { GHOST_BUTTON_CLASS, INPUT_CLASS, PRIMARY_BUTTON_CLASS } from './styles';

  let { provider }: { provider: ProviderDefinition } = $props();

  let settings = $derived(getSettings());
  let tools = $derived(disabledToolsFor(settings, provider.id));
  // The chips and the group render disjoint slices of the same stored list.
  let flatTools = $derived(tools.filter((tool) => !isTodoGroupTool(tool)));
  let exposedTodo = $derived(exposedTodoTools(tools));
  let todoGroupActive = $derived(exposedTodo.length > 0);
  let todoGroupMixed = $derived(
    todoGroupActive && exposedTodo.length < CLAUDE_TODO_TOOL_GROUP.length,
  );
  let todoDetailsOpen = $state(false);
  // The nudge toggle is claudeTodoRemindersDisabled inverted: the switch
  // reads as availability ("nudges on"), matching every other switch here.
  let nudgesOn = $derived(!(settings.claudeTodoRemindersDisabled ?? false));

  let draft = $state('');
  let normalizedDraft = $derived(normalizeToolName(draft));
  let duplicate = $derived(normalizedDraft !== '' && tools.includes(normalizedDraft));
  // The backend refuses a whole UpdateSettings patch over one malformed
  // name, and the caller sees only "Failed to save setting" with the list
  // rolled back under it. Saying which rule the draft breaks, before the
  // write, is the only way the user can act on it.
  let nameError = $derived(disabledToolNameError(draft));
  let canAdd = $derived(normalizedDraft !== '' && !duplicate && nameError === null);
  // One line under the field, whichever refusal applies. A malformed name
  // cannot also be a duplicate — every stored name already passed the same
  // rules — so the order only decides which reads first, not which is lost.
  let draftError = $derived(
    duplicate ? `${normalizedDraft} is already disabled.` : nameError,
  );
  let suggestions = $derived(CLAUDE_TOOL_SUGGESTIONS.filter((name) => !tools.includes(name)));

  async function write(next: string[]): Promise<void> {
    const key = disabledToolsSettingsKey(provider.id);
    if (!key) return;
    await updateSettingsPatch({ [key]: next });
  }

  function addTool(name: string): void {
    // The guard lives here rather than only in the draft path, so no caller
    // can put a name the backend would reject on the wire. Reaching it from
    // the suggestion row would mean a bad literal in CLAUDE_TOOL_SUGGESTIONS
    // — our bug, and one that must not be silent.
    const error = disabledToolNameError(name);
    if (error) {
      console.error(`Refusing to disable ${JSON.stringify(name)}: ${error}`);
      return;
    }
    const next = withToolAdded(tools, name);
    if (next.length === tools.length) return;
    void write(next);
  }

  function addDraft(): void {
    if (!canAdd) return;
    addTool(normalizedDraft);
    draft = '';
  }

  function removeTool(name: string): void {
    void write(withToolRemoved(tools, name));
  }

  function setTodoGroupActive(active: boolean): void {
    void write(
      active
        ? withToolsRemoved(tools, CLAUDE_TODO_TOOL_GROUP)
        : withToolsAdded(tools, CLAUDE_TODO_TOOL_GROUP),
    );
  }

  function setTodoToolAvailable(name: string, available: boolean): void {
    void write(available ? withToolRemoved(tools, name) : withToolAdded(tools, name));
  }

  function setNudgesOn(on: boolean): void {
    void updateSettingsPatch({ claudeTodoRemindersDisabled: !on });
  }

  function handleKeydown(e: KeyboardEvent): void {
    if (e.key !== 'Enter' || isImeComposingEvent(e)) return;
    e.preventDefault();
    addDraft();
  }
</script>

<div data-testid="settings-claude-tools-{provider.id}">
  <SettingsField
    id="claude.disabled-tools"
    label="Disabled tools"
    hint="Their schemas never reach the model. Names are passed to the {provider.cliLabel} verbatim, so a name it doesn't recognise is harmless."
    align="start"
    stacked
  >
    <div class="flex flex-col gap-2.5">
      {#if flatTools.length === 0}
        <p class="text-[0.71875rem] text-fg-hint" data-testid="settings-claude-tools-empty">
          No individual tools disabled.
        </p>
      {:else}
        <div class="flex flex-wrap gap-1.5" data-testid="settings-claude-tools-list">
          {#each flatTools as tool (tool)}
            <span
              class="inline-flex items-center gap-1.5 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 py-0.5 pl-2 pr-1 font-mono text-[0.6875rem] text-fg-muted"
              data-testid="settings-claude-tool-{tool}"
            >
              {tool}
              <button
                type="button"
                class="cursor-pointer rounded-[var(--radius-field)] px-1 text-fg-hint transition-colors hover:text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
                data-testid="settings-claude-tool-remove-{tool}"
                aria-label="Re-enable {tool}"
                onclick={() => removeTool(tool)}
              >
                ×
              </button>
            </span>
          {/each}
        </div>
      {/if}

      <div class="flex items-start gap-2">
        <input
          type="text"
          data-testid="settings-claude-tool-input"
          value={draft}
          placeholder="Tool name"
          autocomplete="off"
          spellcheck="false"
          aria-label="Tool name to disable"
          aria-invalid={draftError !== null}
          oninput={(e) => (draft = (e.currentTarget as HTMLInputElement).value)}
          onkeydown={handleKeydown}
          class="{INPUT_CLASS} max-w-[14rem] font-mono"
        />
        <button
          type="button"
          data-testid="settings-claude-tool-add"
          class={PRIMARY_BUTTON_CLASS}
          disabled={!canAdd}
          onclick={addDraft}
        >
          Add
        </button>
      </div>

      {#if draftError}
        <p class="text-[0.71875rem] text-error" role="alert" data-testid="settings-claude-tool-error">
          {draftError}
        </p>
      {/if}

      {#if suggestions.length > 0}
        <div class="flex flex-wrap items-center gap-1.5">
          <span class="text-[0.6875rem] text-fg-hint">Common:</span>
          {#each suggestions as name (name)}
            <button
              type="button"
              class={GHOST_BUTTON_CLASS + ' font-mono'}
              data-testid="settings-claude-tool-suggest-{name}"
              onclick={() => addTool(name)}
            >
              {name}
            </button>
          {/each}
        </div>
      {/if}

      <!--
        A grouped switch with its own disclosure, not a SettingsField row, so
        it stamps the search index's anchor and label itself. See fields.ts.
      -->
      <div
        class="flex flex-col rounded-[var(--radius-control)] border border-border-subtle/70 bg-surface-0/40"
        data-testid="settings-claude-todo-group"
        data-active={todoGroupActive}
        data-settings-field="claude.todo-tools"
        data-settings-label="Todo tools"
      >
        <div class="flex items-center justify-between gap-4 px-3 py-2">
          <div class="min-w-0">
            <p class="text-[0.75rem] font-medium text-fg">Todo tools</p>
            <p class="mt-0.5 text-[0.6875rem] leading-snug text-fg-muted">
              The task-tracking family (TodoWrite, TaskCreate, TaskUpdate, TaskGet,
              TaskList). One shared list, so they're toggled together.
              {#if todoGroupMixed}
                <span data-testid="settings-claude-todo-mixed">
                  {CLAUDE_TODO_TOOL_GROUP.length - exposedTodo.length} of
                  {CLAUDE_TODO_TOOL_GROUP.length} disabled.
                </span>
              {/if}
            </p>
          </div>
          <div class="flex shrink-0 items-center gap-2">
            <button
              type="button"
              class={GHOST_BUTTON_CLASS}
              data-testid="settings-claude-todo-customize"
              aria-expanded={todoDetailsOpen}
              onclick={() => (todoDetailsOpen = !todoDetailsOpen)}
            >
              {todoDetailsOpen ? 'Collapse' : 'Per-tool'}
            </button>
            <ToggleSwitch
              checked={todoGroupActive}
              ariaLabel="Todo tools available to the model"
              onToggle={setTodoGroupActive}
            />
          </div>
        </div>

        {#if todoDetailsOpen}
          <ul class="flex flex-col gap-1 border-t border-border-subtle/50 px-3 py-2">
            {#each CLAUDE_TODO_TOOL_GROUP as name (name)}
              {@const available = !tools.includes(name)}
              <li
                class="flex items-center justify-between gap-4"
                data-testid="settings-claude-todo-tool-{name}"
                data-available={available}
              >
                <span class="font-mono text-[0.6875rem] text-fg-muted">{name}</span>
                <ToggleSwitch
                  checked={available}
                  ariaLabel="{name} available to the model"
                  onToggle={(value) => setTodoToolAvailable(name, value)}
                />
              </li>
            {/each}
          </ul>
        {/if}

        <div
          class="flex items-center justify-between gap-4 border-t border-border-subtle/50 px-3 py-2"
          data-testid="settings-claude-todo-nudges"
          data-enabled={nudgesOn}
        >
          <div class="min-w-0">
            <p class="text-[0.75rem] font-medium text-fg">Todo nudges</p>
            <p class="mt-0.5 text-[0.6875rem] leading-snug text-fg-muted">
              {#if todoGroupActive}
                Periodic reminders that keep the model updating its task list.
              {:else}
                Nudges already stop when every todo tool is disabled.
              {/if}
            </p>
          </div>
          <ToggleSwitch
            checked={nudgesOn}
            disabled={!todoGroupActive}
            ariaLabel="Todo nudges enabled"
            onToggle={setNudgesOn}
          />
        </div>
      </div>
    </div>
  </SettingsField>
</div>
