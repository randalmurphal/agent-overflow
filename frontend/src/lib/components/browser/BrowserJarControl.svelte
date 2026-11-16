<script lang="ts">
  import Boxes from '@lucide/svelte/icons/boxes';
  import Cookie from '@lucide/svelte/icons/cookie';
  import SquareUser from '@lucide/svelte/icons/square-user';
  import VenetianMask from '@lucide/svelte/icons/venetian-mask';
  import { browserCompanionState, setBrowserJar, type BrowserJar } from '../../stores/browserCompanion.svelte';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import { browserPaneThread } from './browserPaneThread';

  // The browser's cookie jar: which threads' browsers share sign-ins and
  // site data with this one (internal/browser/jar.go). Switching with tabs
  // open closes them and reopens their addresses in the new jar, so it asks
  // first.

  const JARS: { kind: BrowserJar; label: string; description: string; icon: typeof Cookie }[] = [
    { kind: 'worktree', label: 'This worktree', description: 'Shared with every thread on this worktree', icon: Cookie },
    { kind: 'project', label: 'Whole project', description: "Shared with this project's threads that choose it, in any worktree", icon: Boxes },
    { kind: 'thread', label: 'This browser only', description: "This thread's own, kept between sessions", icon: SquareUser },
    { kind: 'private', label: 'Private', description: 'Never saved; gone when its last tab closes', icon: VenetianMask },
  ];

  const thread = browserPaneThread();
  let threadId = $derived(thread());
  let browser = $derived(threadId ? browserCompanionState(threadId) : null);
  let current = $derived(JARS.find((jar) => jar.kind === browser?.jar) ?? JARS[0]);
  // Tabs whose page state a switch drops: every tab with a page behind it or
  // on its way.
  let openTabs = $derived((browser?.pages ?? []).filter((page) => !page.suspended && !page.error).length);

  let anchor: HTMLSpanElement | undefined = $state(undefined);
  let open = $state(false);
  let confirming = $state<(typeof JARS)[number] | null>(null);

  function choose(jar: (typeof JARS)[number]): void {
    open = false;
    if (!threadId || jar.kind === current.kind) return;
    if (openTabs > 0) confirming = jar;
    else void setBrowserJar(threadId, jar.kind);
  }

  function confirm(): void {
    const jar = confirming;
    confirming = null;
    if (threadId && jar) void setBrowserJar(threadId, jar.kind);
  }
</script>

{#if threadId}
  <span bind:this={anchor} class="inline-flex shrink-0">
    <IconButton
      label="Cookie jar: {current.label}"
      size="sm"
      testId="browser-jar"
      ariaHaspopup="menu"
      ariaExpanded={open}
      onClick={() => (open = !open)}
    ><Icon icon={current.icon} size={14} /></IconButton>
  </span>
  <Popover {anchor} {open} onClose={() => (open = false)} placement="bottom-end" role="none">
    <Menu ariaLabel="Cookie jar" minWidthClass="w-[min(19rem,calc(100vw-2rem))] min-w-0" onClose={() => (open = false)}>
      {#each JARS as jar (jar.kind)}
        <MenuItem
          label={jar.label}
          description={jar.description}
          checkbox
          checked={jar.kind === current.kind}
          testId="browser-jar-{jar.kind}"
          onSelect={() => choose(jar)}
        />
      {/each}
    </Menu>
  </Popover>
  <ConfirmDialog
    open={confirming !== null}
    title="Switch to {confirming?.label ?? ''}?"
    description="{openTabs === 1 ? 'The open tab closes' : `The ${openTabs} open tabs close`} and {openTabs === 1 ? 'reopens its address' : 'reopen their addresses'} with the new cookie jar. Page state in {openTabs === 1 ? 'it' : 'them'} is lost: form input, back and forward history, and sign-ins kept in the current jar."
    confirmLabel="Switch and reopen"
    destructive
    onConfirm={confirm}
    onCancel={() => (confirming = null)}
  />
{/if}
