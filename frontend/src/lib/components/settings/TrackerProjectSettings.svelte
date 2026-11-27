<script lang="ts" module>
  import type { BoardChoice, Setup, SetupView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import type { SetupEdit } from '../../stores/trackerSetup.svelte';

  /** Why a page of a Jira project's settings does not apply to a project
   * whose boards are `boards`: it lists none, and a Jira board is added
   * `toDo`, or only Linear ones (`linear`); null when it lists a Jira
   * board. Its setup keeps what it holds for when it lists one (the
   * frame's `none`). */
  export function jiraOnly(boards: readonly BoardChoice[], toDo: string, linear: string): string | null {
    if (boards.some((b) => b.connection_kind === 'jira')) return null;
    return boards.length === 0 ? `This project lists no boards. Add a Jira board in Boards ${toDo}.` : linear;
  }

  /** What a page's body is given: the project, its setup as stored, why
   * a control takes no change (empty when it does), and the save of an
   * edit, whose refusal or failure the frame says, answering whether the
   * project is still the one shown once it settles. */
  export interface TrackerProjectPage {
    readonly project: string;
    readonly view: SetupView;
    readonly setup: Setup;
    readonly lockReason: string;
    readonly save: (edit: SetupEdit) => Promise<boolean>;
  }
</script>

<script lang="ts">
  // A per-project Issues settings page (docs/specs/issues.md, "Setup"):
  // the computer's tracker gate, the project picked, the project's setup
  // file held while the page shows it (stores/trackerSetup), and what the
  // pages share: a setup that cannot be read said with Try again, a
  // section a newer version saved said with no change offered, and a
  // save that lost to another or failed said under the page. Every change
  // saves at once; what shows is what the computer stored. A page adds its
  // own holds (`hold`), their failure (`readError`) and readiness
  // (`ready`), why its settings do not apply to a project (`none`), and
  // renders its body from what the frame gives it.
  import { onMount, untrack, type Snippet } from 'svelte';
  import { grantBlockedTitle } from '../../transport/scopeRefusal';
  import { isLoaded } from '../../stores/projects.svelte';
  import { acquireTrackerConnections, refreshTrackerConnections, trackerConnectionsOf, trackerReadGate } from '../../stores/trackerConnections.svelte';
  import { acquireTrackerSetup, editTrackerSetup, refreshTrackerSetup, trackerSetupOf } from '../../stores/trackerSetup.svelte';
  import { asSentence, userFacingError } from '../../utils/userFacingError';
  import { settingsComputerName } from '../../stores/settings.svelte';
  import SettingsOlderComputer from './SettingsOlderComputer.svelte';
  import TrackerProjectSelect, { computerProjects } from './TrackerProjectSelect.svelte';
  import type { SettingsFieldId } from './fields';
  import { settingsComputer } from './settingsComputer';
  import { GHOST_BUTTON_CLASS } from './styles';

  interface Props {
    /** The page's test ID stem: settings-tracker-<page>. */
    page: string;
    /** The search index's project field and the body's field. */
    projectField: SettingsFieldId;
    bodyField: SettingsFieldId;
    /** The body's heading. */
    heading: string;
    /** The project picker's hint. */
    hint: string;
    /** What the page sets, plural, as its sentences name it ("status
     * groups"). */
    what: string;
    /** The setup section the page changes: one a newer version saved
     * shows no body. */
    section: string;
    /** Holds what else the body reads for `project`; returns its release. */
    hold?: (project: string) => () => void;
    /** Why something else the body reads failed; null when nothing did. */
    readError?: (project: string) => string | null;
    /** Whether what else the body reads has answered. */
    ready?: (project: string) => boolean;
    /** Reads again what else the body reads. */
    retry?: (project: string) => void;
    /** Why the page's settings do not apply to `project`, read once
     * `ready`: said in place of the body and of a newer section's
     * prompt. Null when they apply. */
    none?: (project: string) => string | null;
    /** What shows while the setup or the rest is read. */
    reading: string;
    body: Snippet<[TrackerProjectPage]>;
  }

  let { page, projectField, bodyField, heading, hint, what, section, hold, readError, ready, retry, none, reading, body }: Props = $props();

  const { backend, scopeGrant } = settingsComputer();

  onMount(() => acquireTrackerConnections(backend));

  let projectCount = $derived(computerProjects(backend).length);
  let projectsLoaded = $derived(isLoaded());
  let selectedId = $state('');

  $effect(() => {
    const id = selectedId;
    if (!id) return;
    return untrack(() => {
      const setup = acquireTrackerSetup(id);
      const rest = hold?.(id);
      return () => {
        setup();
        rest?.();
      };
    });
  });

  let connectionsView = $derived(trackerConnectionsOf(backend));
  let gate = $derived(trackerReadGate(connectionsView));
  let setup = $derived(selectedId ? trackerSetupOf(selectedId) : null);
  let newer = $derived(setup?.view?.newer?.includes(section) ?? false);
  let otherError = $derived(selectedId ? readError?.(selectedId) ?? null : null);
  let otherReady = $derived(selectedId ? ready?.(selectedId) ?? true : false);
  let noneNote = $derived(selectedId && otherReady ? none?.(selectedId) ?? null : null);

  let operateGrant = $derived(scopeGrant('tracker:operate'));
  // A control is never disabled for a save or a read in flight, which
  // would take the keyboard's focus from it: the store builds each change
  // from the stored setup, reading it first when it must, and refuses a
  // second save while one runs, which the page says.
  let lockReason = $derived(
    operateGrant === 'granted'
      ? ''
      : operateGrant === 'refused'
        ? `This device may not change a project's ${what} on this computer.`
        : (grantBlockedTitle(operateGrant) ?? ''),
  );

  // What the last save said, for the project it was made for: the
  // computer's refusal or failure.
  let saveNote = $state<{ kind: 'refused'; text: string } | { kind: 'failed'; error: unknown } | null>(null);
  $effect(() => {
    void selectedId;
    saveNote = null;
  });

  /** Saves `edit` of the selected project's setup, saying under the page
   * a refusal or a failure of the save made for the project shown;
   * answers whether that project is still shown. */
  async function save(edit: SetupEdit): Promise<boolean> {
    const project = selectedId;
    saveNote = null;
    try {
      const outcome = await editTrackerSetup(project, edit);
      if (project === selectedId && outcome.kind === 'refused') saveNote = { kind: 'refused', text: asSentence(outcome.sentence) };
    } catch (err) {
      if (project === selectedId) saveNote = { kind: 'failed', error: err };
    }
    return project === selectedId;
  }

  function retryAll(): void {
    void refreshTrackerSetup(selectedId);
    retry?.(selectedId);
  }
</script>

<div class="settings-sections flex flex-col gap-4" data-testid="settings-tracker-{page}">
  {#if gate === 'older'}
    <SettingsOlderComputer {backend} lacks="issue trackers" />
  {:else if gate === 'refused'}
    <p class="text-[0.75rem] text-fg-muted" data-testid="settings-tracker-{page}-refused">
      This device may not read a project's {what} on this computer.
    </p>
  {:else if gate === 'waiting'}
    <p class="text-[0.75rem] text-fg-muted">Asking this computer about its trackers…</p>
  {:else if gate === 'failed'}
    <div class="flex flex-wrap items-center gap-2" role="alert">
      <p class="text-[0.75rem] text-error">{userFacingError(connectionsView.error)}</p>
      <button type="button" class={GHOST_BUTTON_CLASS} onclick={() => void refreshTrackerConnections(backend)}>Try again</button>
    </div>
  {:else if projectsLoaded && projectCount === 0}
    <p class="text-[0.75rem] text-fg-muted" data-testid="settings-tracker-{page}-no-projects">
      No projects yet. Add one from the sidebar first.
    </p>
  {:else}
    <TrackerProjectSelect {backend} field={projectField} testId="settings-tracker-{page}-project" {hint} bind:selected={selectedId} />

    {#if setup}
      <div class="flex flex-col gap-3" data-settings-field={bodyField} data-settings-label={heading}>
        <p class="text-[0.8125rem] font-medium text-fg">{heading}</p>
        {#if setup.error !== null || otherError !== null}
          <div class="flex flex-wrap items-center gap-2" role="alert">
            <p class="text-[0.75rem] text-error" data-testid="settings-tracker-{page}-error">
              {#if setup.error !== null}
                This project's tracker setup cannot be read: {userFacingError(setup.error)}
              {:else}
                {otherError}
              {/if}
            </p>
            <button type="button" class={GHOST_BUTTON_CLASS} onclick={retryAll} data-testid="settings-tracker-{page}-retry">Try again</button>
          </div>
        {/if}
        {#if noneNote !== null}
          <p class="text-[0.75rem] text-fg-muted" data-testid="settings-tracker-{page}-none">{noneNote}</p>
        {:else if newer && (none === undefined || otherReady)}
          <p class="text-[0.75rem] text-warning" data-testid="settings-tracker-{page}-newer">
            A newer version of Agent Overflow saved this project's {what}. They are kept as they are; update the app on {settingsComputerName(backend) || 'this computer'} to change them here.
          </p>
        {:else if setup.view === null || !otherReady}
          {#if setup.error === null && otherError === null}
            <p class="text-[0.75rem] text-fg-muted">{reading}</p>
          {/if}
        {:else}
          {@render body({ project: selectedId, view: setup.view, setup: setup.view.setup, lockReason, save })}
        {/if}
        {#if saveNote}
          <p class="text-[0.75rem] text-error" role="alert" data-testid="settings-tracker-{page}-save-note">
            {saveNote.kind === 'refused' ? `${saveNote.text} Make the change again.` : userFacingError(saveNote.error)}
          </p>
        {/if}
      </div>
    {/if}
  {/if}
</div>
