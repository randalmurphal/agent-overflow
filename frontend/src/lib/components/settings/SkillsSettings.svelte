<script lang="ts">
  // The Skills page (docs/specs/standing-agents.md, "Skills library"): the
  // skills tree that ships with the app, whether each provider home holds
  // it, and Install, Update and Remove per provider. Nothing installs
  // unless the user presses a button here.
  import { onDestroy, onMount, tick } from 'svelte';
  import { settingsComputer } from './settingsComputer';
  import SettingsHeader from './SettingsHeader.svelte';
  import SettingsField from './SettingsField.svelte';
  import { PRIMARY_BUTTON_CLASS, SECONDARY_BUTTON_CLASS, DANGER_BUTTON_CLASS } from './styles';
  import { GetSkillsLibrary, InstallSkills, UpdateSkills, RemoveSkills } from '../../stores/bindings';
  import { onSkillsChanged } from '../../stores/skillsLibrary';
  import { userFacingError } from '../../utils/userFacingError';
  import type { SkillsHome, SkillsLibrary } from '../../../../bindings/agent-overflow/internal/app/models';

  const { call, hasScope } = settingsComputer();

  type Action = 'install' | 'update' | 'remove';
  type FieldId = 'skills.claude' | 'skills.codex';

  const PROVIDERS: Record<string, { label: string; field: FieldId }> = {
    claude: { label: 'Claude Code', field: 'skills.claude' },
    codex: { label: 'Codex', field: 'skills.codex' },
  };

  let library = $state<SkillsLibrary | null>(null);
  let loadError = $state<string | null>(null);
  let busy = $state<Record<string, Action | undefined>>({});
  let actionErrors = $state<Record<string, string | undefined>>({});
  let canWrite = $derived(hasScope('settings:write'));
  let readSeq = 0;

  async function read(): Promise<void> {
    const seq = ++readSeq;
    try {
      const next = await call(() => GetSkillsLibrary());
      if (seq !== readSeq) return;
      library = next;
      loadError = null;
    } catch (err) {
      if (seq !== readSeq) return;
      loadError = userFacingError(err);
    }
  }

  let rows = $state<Record<string, HTMLElement | undefined>>({});

  async function run(provider: string, action: Action): Promise<void> {
    const pressed = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    busy = { ...busy, [provider]: action };
    actionErrors = { ...actionErrors, [provider]: undefined };
    try {
      await call(() =>
        action === 'install' ? InstallSkills(provider) : action === 'update' ? UpdateSkills(provider) : RemoveSkills(provider),
      );
    } catch (err) {
      actionErrors = { ...actionErrors, [provider]: userFacingError(err) };
    } finally {
      busy = { ...busy, [provider]: undefined };
      // A refused change wrote nothing and a failed one may have written
      // part; read the home either way rather than guess.
      await read();
      // The pressed button is gone once the state changes (Install becomes
      // Remove); keep keyboard focus on the row instead of the page body.
      await tick();
      if (pressed && !pressed.isConnected) {
        rows[provider]?.querySelector<HTMLButtonElement>('button:not(:disabled)')?.focus();
      }
    }
  }

  function statusText(home: SkillsHome, shipped: string): string {
    if (home.error) return 'Could not read this home';
    if (!home.installed) return 'Not installed';
    if (!home.complete) return 'Install did not finish';
    if (home.current) return 'Installed';
    return home.version && home.version !== shipped ? `${home.version} installed, update available` : 'Update available';
  }

  const BUSY_LABEL: Record<Action, string> = { install: 'Installing…', update: 'Updating…', remove: 'Removing…' };

  let unsubscribe: (() => void) | null = null;
  onMount(() => {
    unsubscribe = onSkillsChanged(() => void read());
    void read();
  });
  onDestroy(() => {
    unsubscribe?.();
    readSeq++;
  });
</script>

<div class="settings-sections" data-testid="settings-skills">
  <section data-testid="settings-skills-homes">
    {#if library}
      <SettingsHeader
        title={library.name}
        description={`Version ${library.version}, upstream ${library.commit.slice(0, 7)}. Install copies each skill into the provider's skills folder; Remove deletes only what Install wrote.`}
      />
      <div class="flex flex-col gap-1">
        {#each library.homes as home (home.provider)}
          {@const meta = PROVIDERS[home.provider]}
          {@const action = busy[home.provider]}
          {#if meta}
            <SettingsField id={meta.field} label={meta.label}>
              <div
                class="flex flex-wrap items-center justify-end gap-2"
                data-testid="settings-skills-{home.provider}"
                bind:this={rows[home.provider]}
              >
                <span class="text-[0.75rem] text-fg-muted" data-testid="settings-skills-{home.provider}-status">
                  {action ? BUSY_LABEL[action] : statusText(home, library.version)}
                </span>
                {#if !home.error && !home.installed}
                  <button
                    type="button"
                    class={PRIMARY_BUTTON_CLASS}
                    disabled={!canWrite || action !== undefined}
                    data-testid="settings-skills-{home.provider}-install"
                    onclick={() => void run(home.provider, 'install')}
                  >
                    Install
                  </button>
                {/if}
                {#if home.installed && !home.current}
                  <button
                    type="button"
                    class={PRIMARY_BUTTON_CLASS}
                    disabled={!canWrite || action !== undefined}
                    data-testid="settings-skills-{home.provider}-update"
                    onclick={() => void run(home.provider, 'update')}
                  >
                    Update
                  </button>
                {/if}
                {#if home.installed}
                  <button
                    type="button"
                    class={home.current ? SECONDARY_BUTTON_CLASS : DANGER_BUTTON_CLASS}
                    disabled={!canWrite || action !== undefined}
                    data-testid="settings-skills-{home.provider}-remove"
                    onclick={() => void run(home.provider, 'remove')}
                  >
                    Remove
                  </button>
                {/if}
              </div>
            </SettingsField>
            {#if home.error || actionErrors[home.provider]}
              <p class="text-[0.71875rem] text-error" role="alert" data-testid="settings-skills-{home.provider}-error">
                {actionErrors[home.provider] ?? home.error}
              </p>
            {/if}
          {/if}
        {/each}
      </div>
    {:else if loadError}
      <p class="text-[0.75rem] text-error" role="alert" data-testid="settings-skills-load-error">{loadError}</p>
    {:else}
      <p class="text-[0.75rem] text-fg-muted">Loading…</p>
    {/if}
  </section>

  {#if library}
    <section>
      <SettingsHeader title={`${library.skills.length} skills`} />
      <ul class="flex flex-wrap gap-1.5" data-testid="settings-skills-list">
        {#each library.skills as skill (skill)}
          <li
            class="rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2 py-0.5 font-mono text-[0.6875rem] text-fg-muted"
          >
            {skill}
          </li>
        {/each}
      </ul>
    </section>
  {/if}
</div>
