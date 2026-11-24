<script lang="ts">
  // The steps that put a working token in a connection's key file. The
  // token never passes through the app: the person makes it on the
  // tracker's page and pastes it into the key file in their own editor,
  // on the computer that keeps the connection. Both of those open only
  // on that computer (host scope), so a page elsewhere says to do them
  // there and keeps the path copyable; the expiry and the check work from
  // anywhere.
  import type { TrackerConnection } from '../../stores/bindings';
  import CopyButton from '../primitives/CopyButton.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import TrackerTokenExpiry from './TrackerTokenExpiry.svelte';
  import { PRIMARY_BUTTON_CLASS, SECONDARY_BUTTON_CLASS } from './styles';
  import { keyFileSentence } from './trackerConnectionStatus';

  let {
    connection,
    host,
    busy,
    operateReason,
    onOpenTokenPage,
    onOpenKeyFile,
    onSaveExpiry,
    onCheck,
  }: {
    connection: TrackerConnection;
    /** This page runs on the computer that keeps the connection. */
    host: boolean;
    /** An action of this row is running. */
    busy: boolean;
    /** Why this page cannot change connections; '' when it can. */
    operateReason: string;
    onOpenTokenPage: () => void;
    onOpenKeyFile: () => void;
    onSaveExpiry: (expiresAt: number) => Promise<boolean>;
    onCheck: () => void;
  } = $props();

  let makeToken = $derived.by(() => {
    switch (connection.kind) {
      case 'jira':
        return `Create an Atlassian API token while signed in as ${connection.account}. Give it the longest life the page offers.`;
      case 'linear':
        return "Create a personal API key in Linear's security settings.";
      case 'tempo':
        return 'In Jira, open Tempo, then Settings, then API Integration under Data Access, and choose New Token.';
      default:
        return 'Create an API token on the tracker.';
    }
  });
  let tokenPageLabel = $derived(connection.kind === 'tempo' ? 'Open Jira' : 'Open the token page');
</script>

<div class="flex flex-col gap-3 rounded-[var(--radius-field)] border border-border-subtle bg-surface-1/40 p-3" data-testid="tracker-connection-setup">
  {#if !host}
    <p class="text-[0.71875rem] text-fg-muted" data-testid="tracker-connection-setup-remote">
      This page is not on the computer that keeps this connection, so do steps 1 and 2 on that computer.
    </p>
  {/if}
  <ol class="flex list-decimal flex-col gap-3 pl-5 text-[0.75rem] text-fg">
    <li class="flex flex-col gap-1.5">
      <span>{makeToken}</span>
      {#if host}
        <div>
          <button type="button" class={SECONDARY_BUTTON_CLASS} onclick={onOpenTokenPage} data-testid="tracker-connection-open-token-page">
            {tokenPageLabel}
          </button>
        </div>
      {/if}
    </li>
    <li class="flex flex-col gap-1.5">
      <span>Paste the token into this file, as its only content, and save it.</span>
      <div class="flex min-w-0 items-center gap-1">
        <code class="min-w-0 truncate rounded bg-surface-2/60 px-1.5 py-0.5 text-[0.6875rem] text-fg-muted" title={connection.keyFilePath} data-testid="tracker-connection-key-path">{connection.keyFilePath}</code>
        <CopyButton text={connection.keyFilePath} label="Copy the key file's path" size="xs" iconSize={12} onError={() => addToast('error', 'Failed to copy')} />
      </div>
      <span class="text-fg-muted" data-testid="tracker-connection-key-state">{keyFileSentence(connection)}</span>
      {#if host}
        <div>
          <button
            type="button"
            class={SECONDARY_BUTTON_CLASS}
            disabled={busy}
            onclick={onOpenKeyFile}
            data-testid="tracker-connection-open-key-file"
          >Open the key file</button>
        </div>
      {/if}
    </li>
    <li>
      <TrackerTokenExpiry
        expiresAt={connection.tokenExpiresAt}
        disabled={busy || operateReason !== ''}
        disabledReason={operateReason || undefined}
        onSave={onSaveExpiry}
      />
    </li>
    <li class="flex flex-col gap-1.5">
      <span>Check the connection. The app asks the tracker whose token it is.</span>
      <div>
        <button
          type="button"
          class={PRIMARY_BUTTON_CLASS}
          disabled={busy || operateReason !== ''}
          title={operateReason || undefined}
          onclick={onCheck}
          data-testid="tracker-connection-setup-check"
        >Check the connection</button>
      </div>
    </li>
  </ol>
</div>
