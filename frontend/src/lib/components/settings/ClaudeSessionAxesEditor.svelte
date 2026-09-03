<script lang="ts">
  // The three Claude-only axes that reach the CLI through its `--settings`
  // block: output style, subagent limits, and the tool memory limit. None
  // has a CLI flag; the settings key is the delivery mechanism, and Claude's
  // flagSettings source outranks both its own settings files and the
  // subprocess environment. (The peer inbox rides that block too but is its
  // own section — ClaudeCrossSessionEditor.svelte.)
  //
  // All three are read at SPAWN, so an edit reaches the next session rather
  // than the running one — the section says so once, at the top, instead of
  // repeating it per field.
  //
  // Extended thinking sits below them and is the exception: it rides CLI
  // flags at spawn AND a `set_max_thinking_tokens` control request on a
  // running session, so it carries its own note rather than inheriting the
  // section's. The one change that cannot apply live is going back to
  // Claude's default; the field says so when that is what the user picked.
  //
  // Rendered for `claude` only, never `claude-tui`: the interactive TUI is
  // launched through a PTY with no `--settings` flag, so offering these there
  // would advertise settings that cannot reach the binary.

  import { getSettings, updateSettingsPatch } from '../../stores/settings.svelte';
  import {
    CLAUDE_OUTPUT_STYLE_OPTIONS,
    CLAUDE_SUBAGENT_LIMIT_MAX,
    CLAUDE_THINKING_BUDGET_DEFAULT,
    CLAUDE_THINKING_BUDGET_MAX,
    CLAUDE_THINKING_BUDGET_MIN,
    CLAUDE_THINKING_DISPLAY_OPTIONS,
    CLAUDE_THINKING_MODE_OPTIONS,
    CLAUDE_TOOL_MEMORY_LIMIT_MAX_LEN,
    clampSubagentLimit,
    clampThinkingBudget,
    normalizeToolMemoryLimit,
    thinkingChangeDefersRestart,
    thinkingPatch,
    toolMemoryLimitError,
  } from '../../utils/claudeSessionAxes';
  import { createDeferredRestartNotice } from '../../utils/deferredRestartNotice.svelte';
  import type {
    ClaudeSubagentLimits,
    ClaudeThinkingDisplay,
    ClaudeThinkingMode,
  } from '../../types/settings';
  import SettingsField from './SettingsField.svelte';
  import { INPUT_CLASS, SELECT_CLASS } from './styles';

  let settings = $derived(getSettings());

  let outputStyle = $derived(settings.claudeOutputStyle ?? '');
  let limits = $derived<ClaudeSubagentLimits>(settings.claudeSubagentLimits ?? {});
  let spawnDepth = $derived(clampSubagentLimit(limits.maxSpawnDepth ?? 0));
  let maxConcurrent = $derived(clampSubagentLimit(limits.maxConcurrent ?? 0));
  let toolMemoryLimit = $derived(settings.claudeToolMemoryLimit ?? '');

  let thinking = $derived(settings.claudeThinking ?? {});
  let thinkingMode = $derived<ClaudeThinkingMode | ''>(thinking.mode ?? '');
  let thinkingDisplay = $derived<ClaudeThinkingDisplay | ''>(thinking.display ?? '');
  // Only meaningful in budget mode, and the backend drops it elsewhere — so
  // the field seeds from a usable number rather than from a stored 0, which
  // the CLI would read as "thinking disabled".
  let thinkingBudget = $derived(
    thinking.budgetTokens ? clampThinkingBudget(thinking.budgetTokens) : CLAUDE_THINKING_BUDGET_DEFAULT,
  );
  // Only the MODE signs the notice: the transition that defers a restart is
  // a mode transition (thinkingChangeDefersRestart), so a later budget or
  // display edit — which does reach a running session — must not read as
  // "the restart already happened". See deferredRestartNotice.svelte.ts.
  const thinkingRestart = createDeferredRestartNotice(() => thinkingMode);

  // The stored value is always valid — it went through this same check on
  // the way in — so the draft only exists while the field has focus and its
  // content differs from what is stored.
  let memoryDraft = $state<string | null>(null);
  let memoryValue = $derived(memoryDraft ?? toolMemoryLimit);
  let memoryError = $derived(toolMemoryLimitError(memoryValue));

  // The one-line description under each select follows the selection, so the
  // meaning of the chosen value is readable without opening the menu.
  let outputStyleHint = $derived(
    CLAUDE_OUTPUT_STYLE_OPTIONS.find((o) => o.value === outputStyle)?.description ?? '',
  );
  let thinkingModeHint = $derived(
    CLAUDE_THINKING_MODE_OPTIONS.find((o) => o.value === thinkingMode)?.description ?? '',
  );
  let thinkingDisplayHint = $derived(
    CLAUDE_THINKING_DISPLAY_OPTIONS.find((o) => o.value === thinkingDisplay)?.description ?? '',
  );

  // One settings key, so all three parts are written together — patching a
  // part would drop the others.
  function saveThinking(
    mode: ClaudeThinkingMode | '',
    budget: number,
    display: ClaudeThinkingDisplay | '',
  ): void {
    const defers = thinkingChangeDefersRestart(thinkingMode, mode);
    void updateSettingsPatch({ claudeThinking: thinkingPatch(mode, budget, display) });
    // Armed AFTER the optimistic write so it captures the saved mode; a save
    // that is rolled back restores the old one and the notice hides itself.
    if (defers) thinkingRestart.arm();
  }

  function setOutputStyle(value: string): void {
    void updateSettingsPatch({
      claudeOutputStyle: value as (typeof CLAUDE_OUTPUT_STYLE_OPTIONS)[number]['value'],
    });
  }

  // Both axes are written together: they are one settings object, so patching
  // one alone would drop the other.
  function setLimit(axis: 'maxSpawnDepth' | 'maxConcurrent', raw: string): number {
    const parsed = clampSubagentLimit(parseInt(raw, 10));
    const next: ClaudeSubagentLimits = {
      maxSpawnDepth: spawnDepth,
      maxConcurrent: maxConcurrent,
      [axis]: parsed,
    };
    void updateSettingsPatch({ claudeSubagentLimits: next });
    return parsed;
  }

  function commitMemoryLimit(): void {
    const next = normalizeToolMemoryLimit(memoryValue);
    memoryDraft = null;
    // A refused draft is discarded rather than sent: the backend would reject
    // the whole patch, and the field would snap back with only a generic
    // toast to explain it.
    if (toolMemoryLimitError(next) !== null) return;
    if (next === toolMemoryLimit) return;
    void updateSettingsPatch({ claudeToolMemoryLimit: next });
  }
</script>

<div class="flex flex-col gap-1" data-testid="settings-claude-session-axes">
  <p class="text-[0.71875rem] leading-snug text-fg-muted">
    Delivered to Claude Code at session start, so changes apply to new sessions —
    except for thinking, which also reaches sessions already running.
  </p>

  <SettingsField
    id="claude.output-style"
    label="Output style"
    hint="Replaces Claude Code's response style for every session."
    htmlFor="claude-output-style"
  >
    <select
      id="claude-output-style"
      data-testid="settings-claude-output-style"
      class={SELECT_CLASS}
      value={outputStyle}
      onchange={(e) => setOutputStyle((e.currentTarget as HTMLSelectElement).value)}
    >
      {#each CLAUDE_OUTPUT_STYLE_OPTIONS as option (option.value)}
        <option value={option.value}>{option.label}</option>
      {/each}
    </select>
  </SettingsField>
  <p
    class="-mt-1 text-[0.6875rem] leading-snug text-fg-hint"
    data-testid="settings-claude-output-style-hint"
  >
    {outputStyleHint}
  </p>

  <SettingsField
    id="claude.subagent-limits"
    label="Subagent limits"
    hint="How deep subagents may spawn further subagents, and how many may run at once. 0 leaves each to Claude Code."
    align="start"
  >
    <div class="flex items-center gap-3">
      <label class="flex items-center gap-1.5 text-[0.6875rem] text-fg-muted">
        Depth
        <input
          type="number"
          data-testid="settings-claude-subagent-depth"
          min="0"
          max={CLAUDE_SUBAGENT_LIMIT_MAX}
          step="1"
          value={spawnDepth}
          onchange={(e) => {
            const el = e.currentTarget as HTMLInputElement;
            // An out-of-range entry that clamps back to the stored number
            // writes nothing, so the field would keep showing the rejection.
            el.value = String(setLimit('maxSpawnDepth', el.value));
          }}
          class="{INPUT_CLASS} w-16 text-right tabular-nums"
        />
      </label>
      <label class="flex items-center gap-1.5 text-[0.6875rem] text-fg-muted">
        At once
        <input
          type="number"
          data-testid="settings-claude-subagent-concurrent"
          min="0"
          max={CLAUDE_SUBAGENT_LIMIT_MAX}
          step="1"
          value={maxConcurrent}
          onchange={(e) => {
            const el = e.currentTarget as HTMLInputElement;
            el.value = String(setLimit('maxConcurrent', el.value));
          }}
          class="{INPUT_CLASS} w-16 text-right tabular-nums"
        />
      </label>
    </div>
  </SettingsField>

  <SettingsField
    id="claude.thinking"
    label="Thinking"
    hint="How much Claude thinks before answering. A fixed budget only binds on models that take an explicit budget — on models with adaptive thinking, Claude keeps deciding for itself."
    htmlFor="claude-thinking-mode"
  >
    <div class="flex items-center gap-2">
      <select
        id="claude-thinking-mode"
        data-testid="settings-claude-thinking-mode"
        class={SELECT_CLASS}
        value={thinkingMode}
        onchange={(e) =>
          saveThinking(
            (e.currentTarget as HTMLSelectElement).value as ClaudeThinkingMode | '',
            thinkingBudget,
            thinkingDisplay,
          )}
      >
        {#each CLAUDE_THINKING_MODE_OPTIONS as option (option.value)}
          <option value={option.value}>{option.label}</option>
        {/each}
      </select>
      {#if thinkingMode === 'budget'}
        <label class="flex items-center gap-1.5 text-[0.6875rem] text-fg-muted">
          Tokens
          <input
            type="number"
            data-testid="settings-claude-thinking-budget"
            min={CLAUDE_THINKING_BUDGET_MIN}
            max={CLAUDE_THINKING_BUDGET_MAX}
            step="1024"
            value={thinkingBudget}
            onchange={(e) => {
              const el = e.currentTarget as HTMLInputElement;
              // An out-of-range entry clamps back to a stored-equal number
              // and writes nothing, so the field would keep showing the
              // rejection unless it is put back by hand.
              const next = clampThinkingBudget(parseInt(el.value, 10));
              el.value = String(next);
              saveThinking('budget', next, thinkingDisplay);
            }}
            class="{INPUT_CLASS} w-20 text-right tabular-nums"
          />
        </label>
      {/if}
    </div>
  </SettingsField>
  <p
    class="-mt-1 text-[0.6875rem] leading-snug text-fg-hint"
    data-testid="settings-claude-thinking-mode-hint"
  >
    {thinkingModeHint}
    {#if thinkingRestart.visible}
      <span data-testid="settings-claude-thinking-deferred">
        Sessions already running keep their current setting until they next restart.
      </span>
    {:else}
      Applies to running sessions too.
    {/if}
  </p>

  <SettingsField
    id="claude.show-thinking"
    label="Show thinking"
    hint="Whether Claude's thinking text reaches the thread. Claude thinks either way — this only decides whether you see it."
    htmlFor="claude-thinking-display"
  >
    <select
      id="claude-thinking-display"
      data-testid="settings-claude-thinking-display"
      class={SELECT_CLASS}
      value={thinkingDisplay}
      onchange={(e) =>
        saveThinking(
          thinkingMode,
          thinkingBudget,
          (e.currentTarget as HTMLSelectElement).value as ClaudeThinkingDisplay | '',
        )}
    >
      {#each CLAUDE_THINKING_DISPLAY_OPTIONS as option (option.value)}
        <option value={option.value}>{option.label}</option>
      {/each}
    </select>
  </SettingsField>
  <p
    class="-mt-1 text-[0.6875rem] leading-snug text-fg-hint"
    data-testid="settings-claude-thinking-display-hint"
  >
    {thinkingDisplayHint}
  </p>

  <SettingsField
    id="claude.tool-memory-limit"
    label="Tool memory limit"
    hint="Caps memory for the processes Claude's tools spawn — a size like 4G, or none to lift it. Applies only when the backend runs on Linux (WSL included); it is a memory cgroup, which macOS and native Windows have no equivalent for."
    htmlFor="claude-tool-memory-limit"
    align="start"
  >
    <div class="flex flex-col items-end gap-1">
      <input
        id="claude-tool-memory-limit"
        type="text"
        data-testid="settings-claude-tool-memory-limit"
        placeholder="Claude Code default"
        autocomplete="off"
        spellcheck="false"
        maxlength={CLAUDE_TOOL_MEMORY_LIMIT_MAX_LEN}
        aria-invalid={memoryError !== null}
        value={memoryValue}
        oninput={(e) => (memoryDraft = (e.currentTarget as HTMLInputElement).value)}
        onchange={commitMemoryLimit}
        onblur={commitMemoryLimit}
        class="{INPUT_CLASS} w-[10rem] font-mono"
      />
      {#if memoryError}
        <p
          class="text-[0.6875rem] leading-snug text-error"
          role="alert"
          data-testid="settings-claude-tool-memory-limit-error"
        >
          {memoryError}
        </p>
      {/if}
    </div>
  </SettingsField>
</div>
