<script lang="ts">
  // The project's worktree setup recipe: files copied out of the main checkout
  // and commands run in a newly created worktree.
  //
  // Commands are STORED AND EXECUTED AS ARGV — there is no shell (see
  // internal/worktreesetup). The editor still accepts one command line per row,
  // because typing an argv array into a form is miserable; `shellArgv.ts` does
  // the conversion both ways, and a line it cannot represent is an inline error
  // rather than a guess. A recipe that wants a shell asks for one:
  // `sh -c '…'`, which is three arguments.
  //
  // Saving is explicit rather than per-row (the ProviderCustomEnvSection
  // pattern): a half-typed command line is a normal intermediate state here, so
  // a per-keystroke mutator would either persist nonsense or reject every
  // keystroke until the row happened to parse.

  import { GetProjectWorktreeSetup, SetProjectWorktreeSetup } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { tokenizeCommandLine, formatArgv, CommandLineError } from '../../utils/shellArgv';
  import SettingsField from './SettingsField.svelte';
  import StringListEditor from './StringListEditor.svelte';
  import { INPUT_CLASS, PRIMARY_BUTTON_CLASS, SECONDARY_BUTTON_CLASS } from './styles';

  let { projectId }: { projectId: string } = $props();

  type Draft = { copy: string[]; commands: string[]; timeout: string };

  let draft = $state<Draft>({ copy: [], commands: [], timeout: '' });
  let saved = $state<Draft>({ copy: [], commands: [], timeout: '' });
  let loading = $state(true);
  let saving = $state(false);
  let saveError = $state<string | null>(null);

  // One parse per command row. The index positions the inline message; the
  // presence of any entry is what blocks the save.
  let commandErrors = $derived(
    draft.commands.map((line) => {
      if (line.trim() === '') return null;
      try {
        const argv = tokenizeCommandLine(line);
        return argv.length === 0 ? 'Enter a command.' : null;
      } catch (err) {
        return err instanceof CommandLineError ? err.message : 'This command cannot be read.';
      }
    }),
  );
  let timeoutError = $derived(validateTimeout(draft.timeout));
  let dirty = $derived(serialize(draft) !== serialize(saved));
  let canSave = $derived(
    !loading && !saving && dirty && timeoutError === null && commandErrors.every((e) => e === null),
  );

  // Explicit copy rather than structuredClone: reading a $state object hands
  // back a proxy, which structuredClone refuses.
  function cloneDraft(value: Draft): Draft {
    return { copy: [...value.copy], commands: [...value.commands], timeout: value.timeout };
  }

  function serialize(value: Draft): string {
    return JSON.stringify([value.copy, value.commands, value.timeout.trim()]);
  }

  // Mirrors worktreesetup.ResolveTimeout: blank means the 10m default, and
  // anything else must be a positive Go duration.
  function validateTimeout(raw: string): string | null {
    const value = raw.trim();
    if (value === '') return null;
    if (!/^-?(\d+(\.\d*)?|\.\d+)(ns|us|µs|ms|s|m|h)([\d.]+(ns|us|µs|ms|s|m|h))*$/.test(value)) {
      return 'Use a duration such as 10m, 90s, or 1h30m.';
    }
    if (value.startsWith('-') || /^0+(\.0*)?(ns|us|µs|ms|s|m|h)$/.test(value)) {
      return 'The timeout must be greater than zero.';
    }
    return null;
  }

  async function load(): Promise<void> {
    loading = true;
    saveError = null;
    try {
      const config = await GetProjectWorktreeSetup(projectId);
      const next: Draft = {
        copy: [...(config?.copy ?? [])],
        commands: (config?.run ?? []).map((argv) => formatArgv(argv)),
        timeout: config?.timeout ?? '',
      };
      draft = next;
      saved = cloneDraft(next);
    } catch (err) {
      addToast('error', `Failed to load worktree setup: ${errString(err)}`);
    } finally {
      loading = false;
    }
  }

  async function save(): Promise<void> {
    if (!canSave) return;
    saving = true;
    saveError = null;
    try {
      const stored = await SetProjectWorktreeSetup(projectId, {
        copy: draft.copy.map((glob) => glob.trim()).filter((glob) => glob !== ''),
        run: draft.commands
          .filter((line) => line.trim() !== '')
          .map((line) => tokenizeCommandLine(line)),
        timeout: draft.timeout.trim(),
      });
      // Re-seed from what was actually stored, so a recipe the backend
      // normalised (dropped blank rows, cleared an empty config) shows the
      // saved truth instead of the draft that produced it.
      const next: Draft = {
        copy: [...(stored?.copy ?? [])],
        commands: (stored?.run ?? []).map((argv) => formatArgv(argv)),
        timeout: stored?.timeout ?? '',
      };
      draft = next;
      saved = cloneDraft(next);
    } catch (err) {
      saveError = errString(err);
    } finally {
      saving = false;
    }
  }

  function revert(): void {
    draft = cloneDraft(saved);
    saveError = null;
  }

  $effect(() => {
    void projectId;
    void load();
  });
</script>

<div class="space-y-1" data-testid="worktree-setup-editor">
  <SettingsField
    label="Worktree setup"
    hint="Runs whenever this project creates a new worktree. Commands run from the worktree root with AO_PROJECT_ROOT and AO_WORKTREE_PATH set to the two checkouts."
    align="start"
    stacked
  >
    <div class="flex flex-col gap-5">
      <StringListEditor
        label="Copy from the main checkout"
        hint="Project-root-relative globs, e.g. .env or config/*.local. A glob that matches nothing fails setup."
        placeholder=".env"
        testid="worktree-setup-copy"
        addLabel="Add pattern"
        disabled={loading || saving}
        bind:values={draft.copy}
      />

      <StringListEditor
        label="Commands"
        hint="One command per row, run in order. There is no shell — write sh -c '…' if you need one."
        placeholder="pnpm install --frozen-lockfile"
        testid="worktree-setup-run"
        addLabel="Add command"
        disabled={loading || saving}
        errors={commandErrors}
        bind:values={draft.commands}
      />

      <div class="flex flex-col gap-1">
        <label class="text-[0.75rem] font-medium text-fg" for="worktree-setup-timeout">
          Timeout
        </label>
        <p class="text-[0.71875rem] leading-snug text-fg-muted">
          Bounds the whole command sequence. Blank means 10m.
        </p>
        <input
          id="worktree-setup-timeout"
          data-testid="worktree-setup-timeout"
          type="text"
          value={draft.timeout}
          placeholder="10m"
          autocomplete="off"
          spellcheck="false"
          disabled={loading || saving}
          aria-invalid={timeoutError !== null}
          oninput={(e) => (draft.timeout = (e.target as HTMLInputElement).value)}
          class="{INPUT_CLASS} max-w-[10rem] font-mono"
        />
        {#if timeoutError}
          <p class="text-[0.71875rem] text-error" role="alert" data-testid="worktree-setup-timeout-error">
            {timeoutError}
          </p>
        {/if}
      </div>

      {#if saveError}
        <p class="text-[0.71875rem] text-error" role="alert" data-testid="worktree-setup-save-error">
          {saveError}
        </p>
      {/if}

      <div class="flex items-center gap-2">
        <button
          type="button"
          data-testid="worktree-setup-save"
          class={PRIMARY_BUTTON_CLASS}
          disabled={!canSave}
          onclick={() => void save()}
        >
          {saving ? 'Saving…' : 'Save'}
        </button>
        <button
          type="button"
          data-testid="worktree-setup-revert"
          class={SECONDARY_BUTTON_CLASS}
          disabled={loading || saving || !dirty}
          onclick={revert}
        >
          Revert
        </button>
      </div>
    </div>
  </SettingsField>
</div>
