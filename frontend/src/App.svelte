<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { documentHidden } from './lib/utils/pageVisibility';
  import { ensureMainPane, getFocusedPaneOrNull, getPane, iterPanes, openThreadFromNavigation, resetPaneRegistry } from './lib/stores/panes.svelte';
  import { setupEventListeners } from './lib/stores/events';
  import { OPEN_SHIP_CHANGES_EVENT } from './lib/stores/eventNames';
  import { getThreads, loadThreads } from './lib/stores/threads.svelte';
  import { refreshThreadGroups } from './lib/stores/threadGroups.svelte';
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
  import {
    isWorkflowsOverlayOpen,
    syncWorkflowsOverlayFromAppStorage,
  } from './lib/stores/workflowsOverlay.svelte';
  import {
    closeSettingsOverlay,
    getSettingsSection,
    isSettingsOpen,
    openSettingsOverlay,
  } from './lib/stores/settingsOverlay.svelte';
  import { hydrateWorkflowAttention } from './lib/stores/workflowRuns.svelte';
  import { syncSidebarLayoutFromAppStorage } from './lib/stores/sidebarLayout.svelte';
  import { installLayoutMode } from './lib/stores/layoutMode.svelte';
  import {
    installScreenPresence,
    refreshScreenPresence,
  } from './lib/stores/screenPresence';
  import { installLongPressContextMenu } from './lib/utils/longPressContextMenu';
  import { syncUsagePeriodFromSettings } from './lib/stores/usagePeriod.svelte';
  import { preloadProviderModelsForSettings } from './lib/stores/providerModels.svelte';
  import { applyThemeClass } from './lib/utils/theme';
  import { getResolvedTheme } from './lib/stores/themeMode.svelte';
  import {
    getAppearance,
    getAppearanceRevision,
    getAppearanceThemes,
    installAppearanceEvents,
    isAppearanceLoaded,
    loadAppearance,
    syncWindowBackground,
  } from './lib/stores/appearance.svelte';
  import {
    applyTheme,
    getAppliedTheme,
    readWindowGroundHex,
    stampBootTheme,
  } from './lib/theme/themeApply.svelte';
  import { applyFonts } from './lib/utils/fonts';
  import { startAmbientTicker } from './lib/utils/ambientTicker';
  import { startIdleMemoryTrim } from './lib/utils/idleMemoryTrim';
  import { applyFontScale, installZoomKeybindings } from './lib/utils/zoom';
  import Sidebar from './lib/components/sidebar/Sidebar.svelte';
  import PaneHost from './lib/components/panes/PaneHost.svelte';
  import { redirectTypingToFocusedComposer } from './lib/components/panes/typeToFocusComposer';
  import LazyOverlay from './lib/components/primitives/LazyOverlay.svelte';
  import Toast from './lib/components/shared/Toast.svelte';
  import TransportStatusBanner from './lib/components/shared/TransportStatusBanner.svelte';
  import DiscussionStartFlow from './lib/components/discussion/DiscussionStartFlow.svelte';
  import CommandPalette from './lib/components/palette/CommandPalette.svelte';
  import KeybindingsCheatSheet from './lib/components/palette/KeybindingsCheatSheet.svelte';
  import MessageSearch from './lib/components/palette/MessageSearch.svelte';
  import UnifiedThreadPicker from './lib/components/palette/UnifiedThreadPicker.svelte';
  import ThreadActionConfirmationHost from './lib/components/palette/ThreadActionConfirmationHost.svelte';
  import UnsentMessageConfirmationHost from './lib/components/composer/UnsentMessageConfirmationHost.svelte';
  import type { Thread } from './lib/types/models';
  import { getPaletteTargetPaneId, isPaletteOpen } from './lib/stores/palette.svelte';
  import { closeCheatSheet, isCheatSheetOpen } from './lib/stores/cheatSheet.svelte';
  import { closeMessageSearch, getMessageSearchMode, getMessageSearchTargetPaneId, isMessageSearchOpen } from './lib/stores/messageSearch.svelte';
  import { closeThreadPicker, getThreadPickerTargetPaneId, isThreadPickerOpen } from './lib/stores/threadPicker.svelte';
  import { closeAccountSwitcher, isAccountSwitcherOpen } from './lib/stores/accountSwitcher.svelte';
  import { isSessionImportOpen } from './lib/stores/sessionImport.svelte';
  import { isAnyComposerPickerOpen } from './lib/stores/composerPickerRegistry.svelte';
  import {
    dispatchKey,
    eventMatchesKeybindingCommand,
    loadKeybindings,
  } from './lib/stores/keybindings.svelte';
  import { clearCommandRegistry, listCommands, type CommandContext } from './lib/stores/commandRegistry.svelte';
  import { registerBuiltinCommands, makeCommandContext } from './lib/stores/builtinCommands.svelte';
  import { installUiRenderTraceApi } from './lib/utils/uiRenderTrace';
  import { installLoafTrace } from './lib/utils/loafTrace';
  import { installHarnessBridge } from './lib/stores/harnessBridge';
  import { warmHighlightTables } from './lib/utils/syntaxSpans';
  import { dispatchTextEditing } from './lib/utils/textEditingKeymap';
  import { installExternalLinkDelegate } from './lib/utils/externalLinks';
  import { getVisibleSidebarThreadIds } from './lib/stores/sidebarThreadOrder';
  import { setAppShellWidth } from './lib/stores/layoutMetrics.svelte';
  import DiagramInteractionHost from './lib/components/chat/DiagramInteractionHost.svelte';
  import FootnotePopoverHost from './lib/components/chat/FootnotePopoverHost.svelte';
  import ExternalLinkContextHost from './lib/components/shared/ExternalLinkContextHost.svelte';
  import {
    openDraftThreadForProject,
    resolveDraftTargetProject,
  } from './lib/stores/threadCreation.svelte';
  import { addToast } from './lib/stores/toast.svelte';
  import { userFacingError } from './lib/utils/userFacingError';
  import { initUpdates } from './lib/stores/updates.svelte';
  import { initServiceUpdates } from './lib/stores/serviceUpdate.svelte';
  import { initDevServers } from './lib/stores/devServers.svelte';

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

  function makeAppCommandContext(targetPane = getFocusedPaneOrNull()) {
    const pickerOpen =
      isPaletteOpen() ||
      isCheatSheetOpen() ||
      isMessageSearchOpen() ||
      isThreadPickerOpen() ||
      isAccountSwitcherOpen() ||
      isAnyComposerPickerOpen();
    return makeCommandContext(targetPane, {
      paletteOpen: isPaletteOpen(),
      cheatSheetOpen: isCheatSheetOpen(),
      messageSearchOpen: isMessageSearchOpen(),
      threadPickerOpen: isThreadPickerOpen(),
      anyPickerOpen: pickerOpen,
      anyModalOpen:
        discussionStartFor !== null ||
        isSettingsOpen() ||
        isCheatSheetOpen() ||
        isMessageSearchOpen() ||
        isThreadPickerOpen() ||
        isAccountSwitcherOpen(),
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

  // The palette's context walks the active pane and command flags. Keep that
  // dependency graph dormant while the overlay is closed. Streaming item
  // updates replace pane state many times a second, but a hidden palette has
  // nothing to filter or render from those replacements.
  let paletteContext = $derived(
    isPaletteOpen() ? makeAppCommandContext(currentContextPane()) : null,
  );
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
    if (handled) {
      ev.preventDefault();
      return;
    }
    // Unclaimed bare printable key: type-to-focus the focused chat pane's
    // composer. Strictly last so every keybinding — current and future
    // user-configured bare-key chords — wins ahead of it; the helper itself
    // fail-closes on overlays, prompts, traps, and claimed focus.
    redirectTypingToFocusedComposer(ev, ctx.flags);
  }

  function requestThreadJump(index: number): void {
    const ids = getVisibleSidebarThreadIds();
    const targetId = ids[index - 1];
    if (!targetId) return;
    const thread = getThreads().find((t) => t.id === targetId);
    if (thread) void openThreadFromNavigation(thread, getFocusedPaneOrNull() ?? ensureMainPane());
  }

  function requestNewThread(openInNewPane: boolean): void {
    const targetPane = getFocusedPaneOrNull();
    const resolved = resolveDraftTargetProject(targetPane);
    if (!resolved) {
      addToast('warning', 'Add a project before creating a new thread.');
      return;
    }
    void openDraftThreadForProject({
      projectId: resolved.projectId,
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

  // ── The theme is applied in TWO pre-effects and read back in one plain
  // effect, and the split is ordering rather than style. ────────────────────
  //
  // One resolver: `getResolvedTheme()` reads the appearance selection's mode
  // and, for 'system', the single shared prefers-color-scheme subscription.
  // Both are `$state`, so these re-run on a selection flip AND on an OS flip,
  // and the html class lands from the same source the xterm/mermaid consumers
  // read.
  //
  // `$effect.pre` rather than `$effect`: a pre-effect is a RENDER effect, and
  // Svelte flushes every render effect in the tree before any user effect.
  // That is what guarantees the document is fully re-themed before a consumer
  // resolves a palette off the cascade — the mermaid bridge resolves inside
  // `markdown/render/elements/Mermaid.svelte`'s `{@attach}`, and the xterm bridge inside
  // TerminalBody's effect, both of which Svelte builds as user effects.
  //
  // BOTH halves are pre-effects, and the second one is why: the resolved CSS
  // is NOT mode-agnostic. It carries a `:root` block and an `html.light`
  // block, so a theme defining both variants does produce byte-identical text
  // across a mode flip — but a UI theme with only ONE variant is emitted only
  // in the mode it speaks (themeResolve.ts's emission model: the tokens
  // app.css declares as mode-invariant have no light-mode declaration to
  // out-cascade a stale `:root` block, so a dark-only theme left standing in
  // light mode half-applies). Splitting the class stamp into the render pass
  // and the style rewrite into the user pass would therefore leave the whole
  // render pass in a state where the class says one mode and this element
  // still holds the other's resolution.
  $effect.pre(() => {
    applyThemeClass(getResolvedTheme());
  });

  // The `settled` argument is the first-paint guard, not a nicety: this runs
  // at mount with `themes: []`, so a selected USER theme resolves to the
  // built-in fallback until `loadAppearance()` answers. Applying that would
  // overwrite the boot script's cached CSS and clear its inline ground —
  // recreating the flash the stamp exists to prevent, for exactly the users
  // who wrote a theme file. `isAppearanceLoaded()` is a `$state` read, so this
  // re-runs the moment the first answer lands (success, refusal or failure).
  $effect.pre(() => {
    const appearance = getAppearance();
    applyTheme(
      {
        mode: getResolvedTheme(),
        appearance: { uiTheme: appearance.uiTheme, codeTheme: appearance.codeTheme },
        themes: getAppearanceThemes(),
        revision: getAppearanceRevision(),
      },
      isAppearanceLoaded(),
    );
  });

  // Everything that READS the applied cascade back runs here, in the user
  // pass, because it cannot run before the rewrite above has landed: the
  // ground probe forces a style recalc, and `syncWindowBackground` writes
  // store state (which a render effect should not do). The write cannot loop
  // — it only moves `windowBackground`, which re-runs the pre-effect above to
  // an IDENTICAL applied theme, and `applyTheme` does not reassign its state
  // for one.
  $effect(() => {
    // Gated on the same answer as the applier above, and for the same reason
    // in reverse: re-stamping from a pre-load resolution would replace the
    // cached first-paint CSS with the fallback's, so the flash would simply
    // move to the NEXT launch.
    if (!isAppearanceLoaded()) return;
    const applied = getAppliedTheme();
    const ground = readWindowGroundHex();
    stampBootTheme(applied.mode, ground, applied.cssText);
    if (ground) void syncWindowBackground(ground);
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

  // Restate this screen's presence whenever the panes or the compact screen
  // change. The composer reads that state on every run, so the effect keeps
  // its dependencies, and the transport dedups — an unchanged screen writes
  // nothing. The document's own focus and visibility edges are the module's
  // (installScreenPresence below), not this effect's.
  //
  // Read for ONE decision on the backend: whether an OS notification is
  // RAISED. It changes nothing about what this client is sent or renders, and
  // nothing else in the app may read it.
  $effect(() => {
    refreshScreenPresence();
  });

  onMount(() => {
    const cleanupEvents = setupEventListeners();
    // Theme first, and not awaited: the pre-effects above already painted the
    // built-in palette, so this only ever UPGRADES what is on screen — and it
    // has to be in flight before the panes mount so a themed terminal or
    // diagram resolves once rather than twice. The watcher subscription is
    // the agent-edit loop (write themes/*.json, see the app repaint).
    void loadAppearance();
    const cleanupAppearance = installAppearanceEvents();
    const cleanupLayoutMode = installLayoutMode();
    // Passive on-launch update check + updater:* event bridge. No-op on builds
    // without an updater; never downloads or installs without an explicit click.
    const cleanupUpdates = initUpdates();
    // The supervised machines this client can update over the wire: their
    // status on every hello, and their flow frames. Silent where no backend
    // reports a supervisor.
    const cleanupServiceUpdates = initServiceUpdates();
    // Which ports each attached machine will share a preview of, and the
    // two actions the external-link delegate calls. Silent on a machine
    // this session holds no `preview:open` for.
    const cleanupDevServers = initDevServers();
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
        // Groups load beside the threads they contain, but never gate the
        // layout restore below: a failed group load must not be read as a
        // failed boot (refreshThreadGroups owns its own error surface).
        void refreshThreadGroups();
        await appStorageReady;
        syncSidebarLayoutFromAppStorage();
        syncSidebarFromAppStorage();
        // The workflows overlay stack/filter/sweep cursor are durable per
        // client (UI-SPEC §2.1), so they adopt the hydrated copy the same way
        // the sidebar's view state does.
        syncWorkflowsOverlayFromAppStorage();
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
    // Footer attention badge (§6/§7): authoritative on app open, so a missed
    // or dismissed OS notification never loses a parked run. Summaries only.
    void hydrateWorkflowAttention();
    void loadSettingsAndWarmModelCatalogs();
    installUiRenderTraceApi();
    const cleanupLoafTrace = installLoafTrace();
    // Harness ui-query bridge. In an ordinary boot this reads one
    // bootstrap boolean and registers a callback nothing ever fires; the
    // bridge chunk is dynamically imported and stays out of the startup
    // graph. See lib/stores/harnessBridge.ts.
    const cleanupHarnessBridge = installHarnessBridge();
    const cleanupExternalLinks = installExternalLinkDelegate();
    // The phone's right-click: a held touch under the compact layout raises
    // `contextmenu` at the pressed element, so every menu opens on the phone.
    const cleanupLongPress = installLongPressContextMenu();
    const cleanupZoomKeys = installZoomKeybindings();
    const cleanupScreenPresence = installScreenPresence();

    // Register the built-in commands. The hooks close over stable references
    // so commands see the live pane state each time they run.
    clearCommandRegistry();
    registerBuiltinCommands({
      openThreadForm: () => requestNewThread(false),
      openThreadFormInNewPane: () => requestNewThread(true),
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
      if (documentHidden()) {
        flushPaneLayout();
        return;
      }
      // Tab regained focus: requestAnimationFrame resumed. Catch any per-item
      // smoother that fell behind while hidden up to the wire so a turn that
      // streamed (or completed) in the background doesn't crawl in at the
      // smoother's per-tick cap. See ThreadPane.snapSmoothersToReceived.
      for (const pane of iterPanes()) pane.snapSmoothersToReceived();
    };
    window.addEventListener('keydown', handleGlobalKeydown);
    window.addEventListener('pagehide', flushPaneLayout);
    window.addEventListener('beforeunload', flushPaneLayout);
    document.addEventListener('visibilitychange', handleVisibilityChange);

    // Shared wall-clock driver for all ambient indicator visuals
    // (pulse dots, working-LED chase, status glows, stepped spinners).
    const stopAmbientTicker = startAmbientTicker();

    // Report input idleness so the backend can direct a renderer
    // memory-reducing GC between turns (app_webview_trim.go).
    const stopIdleMemoryTrim = startIdleMemoryTrim();

    return () => {
      stopIdleMemoryTrim();
      stopAmbientTicker();
      flushPaneLayout();
      cleanupEvents();
      cleanupAppearance();
      cleanupLayoutMode();
      cleanupUpdates();
      cleanupServiceUpdates();
      cleanupDevServers();
      cleanupExternalLinks();
      cleanupLongPress();
      cleanupZoomKeys();
      cleanupScreenPresence();
      cleanupLoafTrace();
      cleanupHarnessBridge();
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

<main class="app-shell relative h-screen w-screen overflow-hidden text-text-primary flex flex-col">
  <TransportStatusBanner />
  <div bind:this={appContentEl} class="relative flex flex-1 min-h-0 w-full">
    <Sidebar
      pane={sidebarPane}
      onOpenSettings={() => openSettingsOverlay('theme')}
      registerFocusSearch={(focus) => (searchFocuser = focus)}
      registerOpenFromPR={(cb) => (openFromPR = cb)}
    />
    {#if appReady}
      <PaneHost />
    {/if}
    <!--
      Settings and the workflows overlay (UI-SPEC §2.1) are SIBLINGS of
      PaneHost, layered over it — never a pane kind, never a surface that
      replaces the strip. The pane tree stays mounted underneath, so opening
      and closing rebuild nothing. Both load lazily so their chunks stay out
      of the eager startup graph.
    -->
    <LazyOverlay
      load={() => import('./lib/components/settings/SettingsOverlay.svelte')}
      active={isSettingsOpen()}
      props={{
        open: isSettingsOpen(),
        initialSection: getSettingsSection(),
        onClose: closeSettingsOverlay,
      }}
    />
    <LazyOverlay
      load={() => import('./lib/components/workflows/WorkflowsOverlay.svelte')}
      active={isWorkflowsOverlayOpen()}
      props={{ open: isWorkflowsOverlayOpen() }}
    />
  </div>
</main>
<DiscussionStartFlow
  open={discussionStartFor !== null}
  thread={discussionStartFor}
  onClose={closeDiscussionStart}
/>
<CommandPalette context={paletteContext} contextForPane={makeCommandContextForPaneId} />
<ThreadActionConfirmationHost />

<UnsentMessageConfirmationHost />
<KeybindingsCheatSheet open={isCheatSheetOpen()} onClose={closeCheatSheet} />
<MessageSearch open={isMessageSearchOpen()} pane={messageSearchPane} mode={getMessageSearchMode()} onClose={closeMessageSearch} />
<UnifiedThreadPicker open={isThreadPickerOpen()} pane={threadPickerPane} onClose={closeThreadPicker} />
<!--
  Account switcher loads lazily: it is a rarely-mounted picker that pulls in the
  provider-account store and the quota bars, none of which belong in the eager
  startup graph.
-->
<LazyOverlay
  load={() => import('./lib/components/accounts/AccountSwitcher.svelte')}
  active={isAccountSwitcherOpen()}
  props={{ open: isAccountSwitcherOpen(), onClose: closeAccountSwitcher }}
/>
<!--
  Session import: a whole feature chunk (catalogue list, virtualized rows,
  filters) that most sessions never open, so it loads on first use. It takes
  no props — `open` and the close guard both live in the store, so there is
  no second copy of that state for a call site to disagree with.

  Mounted here rather than next to its sidebar trigger: Sidebar renders
  ProjectsSection only while expanded, so a mod+b collapse would unmount a
  run in progress.
-->
<LazyOverlay
  load={() => import('./lib/components/import/SessionImportModal.svelte')}
  active={isSessionImportOpen()}
  props={{}}
/>
<Toast />
<DiagramInteractionHost />
<FootnotePopoverHost />
<ExternalLinkContextHost />
