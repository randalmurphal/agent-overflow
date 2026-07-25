<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { ensureMainPane, getFocusedPaneOrNull, getPane, iterPanes, openThreadFromNavigation, resetPaneRegistry } from './lib/stores/panes.svelte';
  import {
    OPEN_SETTINGS_EVENT,
    OPEN_SHIP_CHANGES_EVENT,
    RENAME_THREAD_EVENT,
    setupEventListeners,
  } from './lib/stores/events';
  import { getThreads, loadThreads } from './lib/stores/threads.svelte';
  import { markNotificationHydrated } from './lib/stores/eventsNotification';
  import {
    installPaneLayoutPersistence,
    loadPersistedPaneLayout,
  } from './lib/stores/paneLayoutPersistence';
  import { flushPaneLayoutPersistence, setPaneLayoutItems } from './lib/stores/paneLayout.svelte';
  import { installCompanionPanes } from './lib/stores/companionPanes.svelte';
  import { flushAppStorage, hydrateAppStorage } from './lib/stores/appStorage';
  import { loadSettings, getSettings } from './lib/stores/settings.svelte';
  import { syncSidebarFromAppStorage, syncSidebarFromSettings } from './lib/stores/sidebar.svelte';
  import { syncSidebarWidthFromAppStorage } from './lib/stores/sidebarLayout.svelte';
  import { syncUsagePeriodFromSettings } from './lib/stores/usagePeriod.svelte';
  import { preloadProviderModelsForSettings } from './lib/stores/providerModels.svelte';
  import { applyTheme } from './lib/utils/theme';
  import { applyFonts } from './lib/utils/fonts';
  import { startAmbientTicker } from './lib/utils/ambientTicker';
  import { applyFontScale, installZoomKeybindings } from './lib/utils/zoom';
  import Sidebar from './lib/components/sidebar/Sidebar.svelte';
  import PaneHost from './lib/components/panes/PaneHost.svelte';
  import Toast from './lib/components/shared/Toast.svelte';
  import TransportStatusBanner from './lib/components/shared/TransportStatusBanner.svelte';
  import DiscussionStartFlow from './lib/components/discussion/DiscussionStartFlow.svelte';
  import CommandPalette from './lib/components/palette/CommandPalette.svelte';
  import KeybindingsCheatSheet from './lib/components/palette/KeybindingsCheatSheet.svelte';
  import MessageSearch from './lib/components/palette/MessageSearch.svelte';
  import UnifiedThreadPicker from './lib/components/palette/UnifiedThreadPicker.svelte';
  import ThreadActionConfirmationHost from './lib/components/palette/ThreadActionConfirmationHost.svelte';
  import type { Thread } from './lib/types/models';
  import { getPaletteTargetPaneId, isPaletteOpen } from './lib/stores/palette.svelte';
  import { closeCheatSheet, isCheatSheetOpen } from './lib/stores/cheatSheet.svelte';
  import { closeMessageSearch, getMessageSearchMode, getMessageSearchTargetPaneId, isMessageSearchOpen } from './lib/stores/messageSearch.svelte';
  import { closeThreadPicker, getThreadPickerTargetPaneId, isThreadPickerOpen } from './lib/stores/threadPicker.svelte';
  import { isAnyComposerPickerOpen } from './lib/stores/composerPickerRegistry.svelte';
  import {
    dispatchKey,
    eventMatchesKeybindingCommand,
    loadKeybindings,
  } from './lib/stores/keybindings.svelte';
  import { clearCommandRegistry, listCommands, type CommandContext } from './lib/stores/commandRegistry.svelte';
  import { registerBuiltinCommands, makeCommandContext } from './lib/stores/builtinCommands.svelte';
  import { installUiRenderTraceApi } from './lib/utils/uiRenderTrace';
  import { warmHighlightTables } from './lib/utils/syntaxSpans';
  import { dispatchTextEditing } from './lib/utils/textEditingKeymap';
  import { installExternalLinkDelegate } from './lib/utils/externalLinks';
  import { getVisibleSidebarThreadIds } from './lib/stores/sidebarThreadOrder';
  import { setAppShellWidth } from './lib/stores/layoutMetrics.svelte';
  import DiagramInteractionHost from './lib/components/chat/DiagramInteractionHost.svelte';
  import {
    openDraftThreadForProject,
    resolveDraftTargetProject,
    type DraftMode,
  } from './lib/stores/threadCreation.svelte';
  import { addToast } from './lib/stores/toast.svelte';
  import { userFacingError } from './lib/utils/userFacingError';
  import { initUpdates } from './lib/stores/updates.svelte';
  import type { SettingsSection } from './lib/components/settings/sections';

  type SettingsContextTarget = {
    threadId?: string;
    provider: string;
    model: string;
    contextWindow?: number;
    autoCompactStandardPercent?: number;
    autoCompactExtendedPercent?: number;
  } | null;

  let showSettings = $state(false);
  let settingsSection = $state<SettingsSection>('general');
  let settingsContextTarget = $state<SettingsContextTarget>(null);
  let discussionStartFor = $state<Thread | null>(null);
  let searchFocuser = $state<(() => void) | null>(null);
  let openFromPR = $state<(() => void) | null>(null);
  let appContentEl: HTMLDivElement | undefined = $state(undefined);
  let appReady = $state(false);
  let paneLayoutRestored = $state(false);

  let sidebarPane = $derived(getFocusedPaneOrNull());
  // Commands that fire even when focus sits inside an INPUT / TEXTAREA /
  // contentEditable. Each command self-declares the flag at registration
  // (see commandRegistry's `editableReachable`); the set is derived from
  // the live registry so adding a new editable-reachable command requires
  // only the per-command flag, not a parallel list here. The
  // textarea-word-op fallback (Option/Alt+Arrow / Ctrl+Arrow) deliberately
  // yields to these chords when a keybinding matches — see handleGlobalKeydown
  // below. The shipped pane-nav defaults avoid Option/Alt+Arrow on macOS, but
  // user rebinds still pass through this same configurable path.
  let EDITABLE_REACHABLE_COMMANDS = $derived(
    new Set(listCommands().filter((c) => c.editableReachable).map((c) => c.id)),
  );

  function handleStartDiscussion(thread: Thread): void {
    discussionStartFor = thread;
  }

  function closeDiscussionStart(): void {
    discussionStartFor = null;
  }

  function openSettings(section: SettingsSection = settingsSection, contextTarget: SettingsContextTarget = null): void {
    settingsSection = section;
    settingsContextTarget = contextTarget;
    showSettings = true;
  }

  function makeAppCommandContext(targetPane = getFocusedPaneOrNull()) {
    const pickerOpen =
      isPaletteOpen() ||
      isCheatSheetOpen() ||
      isMessageSearchOpen() ||
      isThreadPickerOpen() ||
      isAnyComposerPickerOpen();
    return makeCommandContext(targetPane, {
      paletteOpen: isPaletteOpen(),
      cheatSheetOpen: isCheatSheetOpen(),
      messageSearchOpen: isMessageSearchOpen(),
      threadPickerOpen: isThreadPickerOpen(),
      anyPickerOpen: pickerOpen,
      anyModalOpen:
        discussionStartFor !== null ||
        showSettings ||
        isCheatSheetOpen() ||
        isMessageSearchOpen() ||
        isThreadPickerOpen(),
    });
  }

  function makeCommandContextForPaneId(paneId: string): CommandContext | null {
    const targetPane = getPane(paneId);
    return targetPane ? makeAppCommandContext(targetPane) : null;
  }

  // Resolve the pane an overlay/command targets: the overlay's pinned pane when
  // it's open, otherwise the focused pane. Each overlay pins its own target so
  // command enablement tracks the pane the user invoked from, not wherever focus
  // later drifted (frontend/AGENTS.md: "resolve against an explicit target pane").
  function resolveContextPane(targetPaneId: string | null) {
    return getPane(targetPaneId ?? '') ?? getFocusedPaneOrNull();
  }

  // Shared by the reactive `paletteContext` $derived and the fresh per-keypress
  // build in handleGlobalKeydown so both resolve a command's target pane the
  // same way (resolveContextPane above documents the palette-target-else-focused
  // rule).
  function currentContextPane() {
    return resolveContextPane(getPaletteTargetPaneId());
  }

  let paletteContext = $derived(makeAppCommandContext(currentContextPane()));
  let messageSearchPane = $derived(resolveContextPane(getMessageSearchTargetPaneId()));
  let threadPickerPane = $derived(resolveContextPane(getThreadPickerTargetPaneId()));

  function handleGlobalKeydown(ev: KeyboardEvent): void {
    if (ev.defaultPrevented) return;
    // Let free-text inputs keep their typing behaviour. The palette overlay
    // mounts its own input handler that bypasses this branch naturally.
    const target = ev.target as HTMLElement | null;
    const tag = target?.tagName;
    const editable =
      tag === 'INPUT' ||
      tag === 'TEXTAREA' ||
      tag === 'SELECT' ||
      target?.isContentEditable === true;

    // Build the context FRESH for dispatch rather than reading the memoized
    // `paletteContext` $derived. The terminal-focus signal is a plain counter,
    // not $state (terminalStore.svelte.ts), so focusing an xterm while its pane
    // is already focused changes nothing the $derived tracks — it would stay
    // stale-false. A stale `terminalFocus` lets `terminalFocus`-gated chords
    // misfire from inside the terminal: most visibly `pane.close` (mod+w), which
    // is `editableReachable` and guarded only by `when: '!terminalFocus'`, would
    // close the pane instead of passing ctrl-w through as the shell's werase.
    const ctx = makeAppCommandContext(currentContextPane());

    // Editable target: a keybinding in EDITABLE_REACHABLE_COMMANDS wins
    // ahead of the word-op fallback. This is what lets configured editable
    // commands fire from inside the composer; for chords that DON'T match a
    // reachable command (alt+backspace, ctrl+arrow, alt+arrow by default, etc.)
    // the word-op fallback still runs.
    if (editable) {
      if (eventMatchesKeybindingCommand(ev, ctx, EDITABLE_REACHABLE_COMMANDS)) {
        if (dispatchKey(ev, ctx)) ev.preventDefault();
        return;
      }
      if (dispatchTextEditing(ev)) {
        ev.preventDefault();
      }
      return;
    }

    // Non-editable target: word-op fallback is a no-op (its own
    // editableTarget gate returns null), so go straight to dispatch.
    const handled = dispatchKey(ev, ctx);
    if (handled) ev.preventDefault();
  }

  function requestRenameForThread(thread: Thread): void {
    // Sidebar currently owns inline rename via ThreadRow. We surface the
    // rename request as a CustomEvent so the (future) rename modal or the
    // sidebar row can pick it up; until then the event is documentation.
    window.dispatchEvent(new CustomEvent(RENAME_THREAD_EVENT, { detail: thread }));
  }

  function requestThreadJump(index: number): void {
    const ids = getVisibleSidebarThreadIds();
    const targetId = ids[index - 1];
    if (!targetId) return;
    const thread = getThreads().find((t) => t.id === targetId);
    if (thread) void openThreadFromNavigation(thread, getFocusedPaneOrNull() ?? ensureMainPane());
  }

  function requestNewThread(openInNewPane: boolean, mode: DraftMode = 'chat'): void {
    const targetPane = getFocusedPaneOrNull();
    const resolved = resolveDraftTargetProject(targetPane, mode);
    if (!resolved) {
      addToast('warning', 'Add a project before creating a new thread.');
      return;
    }
    void openDraftThreadForProject({
      projectId: resolved.projectId,
      mode: resolved.mode,
      targetPane,
      openInNewPane,
    }).catch((err) => {
      console.error('Failed to create draft thread:', err);
      addToast('error', userFacingError(err));
    });
  }

  async function loadSettingsAndWarmModelCatalogs(): Promise<void> {
    const loaded = await loadSettings();
    if (!loaded) return;
    syncSidebarFromSettings();
    syncUsagePeriodFromSettings();
    void preloadProviderModelsForSettings(getSettings());
  }

  $effect(() => {
    applyTheme(getSettings().theme);
  });

  $effect(() => {
    const s = getSettings();
    applyFonts(s.sansFont, s.monoFont);
  });

  $effect(() => {
    applyFontScale(getSettings().fontSize);
  });

  $effect(() => {
    const el = appContentEl;
    if (!el) return;
    const obs = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry) setAppShellWidth(entry.contentRect.width);
    });
    obs.observe(el);
    setAppShellWidth(el.getBoundingClientRect().width);
    return () => obs.disconnect();
  });

  onMount(() => {
    const cleanupEvents = setupEventListeners();
    // Passive on-launch update check + updater:* event bridge. No-op on builds
    // without an updater; never downloads or installs without an explicit click.
    const cleanupUpdates = initUpdates();
    // appStorage hydration gates the view-state consumers: pane layout
    // restore reads the per-client bucket, and the sidebar syncs adopt
    // the durable copies over the pre-hydration cache. A failed
    // hydration (offline backend) leaves the same-session cache in
    // charge, which is the correct degraded behavior.
    const appStorageReady = hydrateAppStorage();
    // Highlight schema-version + class-name tables, warmed in parallel
    // with the layout restore and awaited (bounded) before PaneHost
    // mounts, so the first history rows' persisted-span ingest seeds
    // synchronously (utils/persistedSpans.ts) instead of deferring past
    // their children's cache reads and RPCing anyway. Never rejects;
    // the race deadline keeps a hung backend from holding boot hostage
    // (past it, the first rows fall back to the RPC path).
    const highlightTablesWarm = warmHighlightTables();
    void (async () => {
      let threadRegistryHydrated = false;
      try {
        const threads = await loadThreads();
        threadRegistryHydrated = true;
        await appStorageReady;
        syncSidebarWidthFromAppStorage();
        syncSidebarFromAppStorage();
        await loadPersistedPaneLayout(threads);
        installPaneLayoutPersistence();
        installCompanionPanes();
        await Promise.race([
          highlightTablesWarm,
          new Promise((resolve) => setTimeout(resolve, 1500)),
        ]);
      } catch (err) {
        console.error('Failed to restore pane layout:', err);
        setPaneLayoutItems([]);
        resetPaneRegistry(null);
        addToast('error', userFacingError(err, 'Failed to restore pane layout.'));
      } finally {
        if (threadRegistryHydrated) await markNotificationHydrated();
        paneLayoutRestored = true;
        appReady = true;
      }
    })();
    void loadSettingsAndWarmModelCatalogs();
    installUiRenderTraceApi();
    const cleanupExternalLinks = installExternalLinkDelegate();
    const cleanupZoomKeys = installZoomKeybindings();

    // Register the built-in commands. The hooks close over stable references
    // so commands see the live pane state each time they run.
    clearCommandRegistry();
    registerBuiltinCommands({
      openSettings: () => openSettings('general'),
      openThreadForm: () => requestNewThread(false),
      openThreadFormInNewPane: () => requestNewThread(true),
      openDesignThreadForm: () => requestNewThread(false, 'design'),
      openDesignThreadFormInNewPane: () => requestNewThread(true, 'design'),
      openThreadFromPR: () => {
        // Sidebar registers a callback that flips its local `showFromPR`
        // state. The indirection keeps dialog ownership with the sidebar,
        // same pattern as focusThreadSearch above.
        openFromPR?.();
      },
      openShipChanges: (paneId) => {
        // The Ship Changes drawer lives inside GitActionsControl (deep in
        // the chat tree). A CustomEvent keeps App.svelte from owning a
        // reference to a component it never renders.
        window.dispatchEvent(new CustomEvent(OPEN_SHIP_CHANGES_EVENT, {
          detail: { paneId },
        }));
      },
      requestRename: requestRenameForThread,
      requestDiscussion: handleStartDiscussion,
      focusThreadSearch: () => searchFocuser?.(),
      requestThreadJump,
    });

    void loadKeybindings();
    const flushPaneLayout = () => {
      if (paneLayoutRestored) void flushPaneLayoutPersistence();
      // Deliver any debounced appStorage writes (sidebar view state,
      // width) before the page goes away. Best-effort — the WS send
      // may not complete on a hard kill, and the same-session cache
      // covers the reload case regardless.
      void flushAppStorage();
    };
    const handleVisibilityChange = () => {
      if (document.visibilityState === 'hidden') {
        flushPaneLayout();
        return;
      }
      // Tab regained focus: requestAnimationFrame resumed. Catch any per-item
      // smoother that fell behind while hidden up to the wire so a turn that
      // streamed (or completed) in the background doesn't crawl in at the
      // smoother's per-tick cap. See ThreadPane.snapSmoothersToReceived.
      for (const pane of iterPanes()) pane.snapSmoothersToReceived();
    };
    const handleOpenSettings = (event: Event) => {
      const detail = (event as CustomEvent).detail as {
        section?: SettingsSection;
        contextTarget?: NonNullable<SettingsContextTarget>;
      } | undefined;
      openSettings(detail?.section ?? 'general', detail?.contextTarget ?? null);
    };
    window.addEventListener(OPEN_SETTINGS_EVENT, handleOpenSettings);
    window.addEventListener('keydown', handleGlobalKeydown);
    window.addEventListener('pagehide', flushPaneLayout);
    window.addEventListener('beforeunload', flushPaneLayout);
    document.addEventListener('visibilitychange', handleVisibilityChange);

    // Shared wall-clock driver for all ambient indicator visuals
    // (pulse dots, working-LED chase, status glows, stepped spinners).
    const stopAmbientTicker = startAmbientTicker();

    return () => {
      stopAmbientTicker();
      flushPaneLayout();
      cleanupEvents();
      cleanupUpdates();
      cleanupExternalLinks();
      cleanupZoomKeys();
      window.removeEventListener(OPEN_SETTINGS_EVENT, handleOpenSettings);
      window.removeEventListener('pagehide', flushPaneLayout);
      window.removeEventListener('beforeunload', flushPaneLayout);
      document.removeEventListener('visibilitychange', handleVisibilityChange);
    };
  });

  onDestroy(() => {
    if (typeof window !== 'undefined') {
      window.removeEventListener('keydown', handleGlobalKeydown);
    }
    clearCommandRegistry();
  });
</script>

{#snippet settingsSurface()}
  <!-- Lazy: the settings surface only exists while open, so its chunk
       stays out of the eager startup graph. -->
  {#await import('./lib/components/settings/SettingsView.svelte')}
    <div class="flex h-full items-center justify-center text-xs text-fg-muted">Loading settings...</div>
  {:then { default: SettingsView }}
    <SettingsView
      initialSection={settingsSection}
      contextTarget={settingsContextTarget}
      onClose={() => showSettings = false}
    />
  {:catch err}
    <div class="flex h-full items-center justify-center text-xs text-error" data-testid="settings-load-error">
      Failed to load settings: {err instanceof Error ? err.message : String(err)}
    </div>
  {/await}
{/snippet}

<main class="app-shell relative h-screen w-screen overflow-hidden text-text-primary flex flex-col">
  <TransportStatusBanner />
  <div bind:this={appContentEl} class="relative flex flex-1 min-h-0 w-full">
    <Sidebar
      pane={sidebarPane}
      onOpenSettings={() => openSettings('general')}
      registerFocusSearch={(focus) => (searchFocuser = focus)}
      registerOpenFromPR={(cb) => (openFromPR = cb)}
    />
    {#if appReady}
      <PaneHost globalSurface={showSettings ? settingsSurface : undefined} />
    {/if}
  </div>
</main>
<DiscussionStartFlow
  open={discussionStartFor !== null}
  thread={discussionStartFor}
  onClose={closeDiscussionStart}
/>
<CommandPalette context={paletteContext} contextForPane={makeCommandContextForPaneId} />
<ThreadActionConfirmationHost />
<KeybindingsCheatSheet open={isCheatSheetOpen()} onClose={closeCheatSheet} />
<MessageSearch open={isMessageSearchOpen()} pane={messageSearchPane} mode={getMessageSearchMode()} onClose={closeMessageSearch} />
<UnifiedThreadPicker open={isThreadPickerOpen()} pane={threadPickerPane} onClose={closeThreadPicker} />
<Toast />
<DiagramInteractionHost />
