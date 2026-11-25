<script lang="ts">
  // Link ticket… and Change ticket… (docs/specs/issues.md, "Thread link"):
  // the ticket finder over the thread's computer's Jira and Linear
  // connections that are turned on, which links the thread to the ticket
  // picked. Loaded on the first use by ThreadTicketFinderHost, so linking
  // adds nothing to startup. A computer with no such connection, a release
  // with no trackers and a computer that refuses this device say so and
  // close.
  import TicketFinder, { type FinderSource, type FoundTicket } from './TicketFinder.svelte';
  import { trackerConnectionName, trackerKindName } from '../settings/trackerConnectionStatus';
  import { acquireTrackerConnections, trackerConnectionsOf, trackerReadGate } from '../../stores/trackerConnections.svelte';
  import { backendDisplayName, attachedBackendEntry, threadMachine } from '../../stores/attachedBackends.svelte';
  import { closeThreadTicketFinder, linkThreadTicket, type ThreadTicketFinding } from '../../stores/threadTicketLinks.svelte';
  import { addErrorToast, addToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';

  let { finding }: { finding: ThreadTicketFinding } = $props();

  const LINKABLE = new Set(['jira', 'linear']);

  const backend = $derived(threadMachine(finding.thread.id, finding.thread.projectId));
  $effect(() => acquireTrackerConnections(backend));
  const view = $derived(trackerConnectionsOf(backend));
  const gate = $derived(trackerReadGate(view));
  const connections = $derived(view.list.filter((c) => LINKABLE.has(c.kind) && c.enabled));
  const sources: FinderSource[] = $derived(connections.map((c) => ({ backend, connection: c.id, name: trackerConnectionName(c) })));

  function computerName(): string {
    const entry = attachedBackendEntry(backend);
    return entry ? backendDisplayName(entry) : 'The thread\'s computer';
  }

  // What cannot be linked is said once, and the finder closes.
  $effect(() => {
    if (gate === 'waiting' || gate === 'open' && sources.length > 0) return;
    if (gate === 'failed') addErrorToast(`${computerName()} could not list its tracker connections: ${userFacingError(view.error)}`, view.error);
    else if (gate === 'older') addToast('error', `${computerName()} runs a release without Issues, so its threads cannot be linked to tickets.`);
    else if (gate === 'refused') addToast('error', `${computerName()} does not let this device read its tickets.`);
    else addToast('error', `${computerName()} has no ${trackerKindName('jira')} or ${trackerKindName('linear')} connection turned on. Add one in Settings > Issues > Connections.`);
    closeThreadTicketFinder();
  });

  function pick(ticket: FoundTicket): void {
    const connection = connections.find((c) => c.id === ticket.connection);
    closeThreadTicketFinder();
    // The list read again while the finder was open, without it.
    if (!connection) {
      addToast('error', `The thread was not linked to ${ticket.key || 'the ticket'}: its connection is no longer turned on on ${computerName()}.`);
      return;
    }
    void linkThreadTicket(finding.thread, { kind: connection.kind, site: connection.site, issueId: ticket.issue, key: ticket.key });
  }
</script>

{#if gate === 'open' && sources.length > 0}
  <TicketFinder anchor={finding.anchor} open {sources} onPick={pick} onClose={closeThreadTicketFinder} />
{/if}
