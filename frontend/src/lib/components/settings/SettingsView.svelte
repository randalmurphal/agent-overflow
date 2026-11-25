<script module lang="ts">
  import { Version } from '../../stores/bindings';

  // App version is static for the lifetime of a build; cache once resolved so
  // reopening Settings does not issue a redundant Version() RPC.
  let cachedVersion: string | null = null;
  let versionPromise: Promise<string> | null = null;

  function loadAppVersion(): Promise<string> {
    if (cachedVersion !== null) return Promise.resolve(cachedVersion);
    versionPromise ??= Version()
      .then((v: string) => {
        if (v) {
          cachedVersion = v;
        } else {
          versionPromise = null;
        }
        return v;
      })
      .catch(() => {
        versionPromise = null;
        return '';
      });
    return versionPromise;
  }
</script>

<script lang="ts">
  // The Settings surface: header, nav rail (SettingsRail), and the page
  // panel. Pages load lazily through `pageLoaders.ts` (sync on warm revisit);
  // the panel renders the active page's title and description from
  // `sections.ts` above the page component so no page repeats its own name.
  //
  // A search hit opens its page and, once the page has rendered, scrolls
  // to and flashes the field. The reveal runs after `tick()` (the page is
  // in the DOM) and then a frame later (layout has settled), scoped to the
  // panel so it can only ever match the page that is actually mounted.

  import { tick, untrack, type Component } from 'svelte';
  import ChevronLeft from '@lucide/svelte/icons/chevron-left';
  import X from '@lucide/svelte/icons/x';
  import Icon from '../primitives/Icon.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import {
    hideSettingsRail,
    getSettingsComputer,
    setSettingsComputer,
    isSettingsRailOpen,
    showSettingsRail,
  } from '../../stores/settingsOverlay.svelte';
  import MicroLabel from '../primitives/MicroLabel.svelte';
  import SettingsRail from './SettingsRail.svelte';
  import { SETTINGS_PAGE_LOADERS } from './pageLoaders';
  import {
    DEFAULT_SETTINGS_SECTION,
    settingsSectionDef,
    settingsSectionDescription,
    settingsUsesComputer,
    type SettingsSection,
  } from './sections';
  import { revealSettingsField, type SettingsSearchHit } from './settingsSearch';
  import { PRIMARY_BUTTON_CLASS, SECTION_PROSE_CLASS } from './styles';
  import { remoteAccessAvailable } from '../../transport/buildVariant';
  import ComputerSelect from '../primitives/ComputerSelect.svelte';
  import ComputerSettingsPage from './ComputerSettingsPage.svelte';
  import { selectedBackend } from '../../stores/selectedBackend.svelte';
  import { hasMultipleBackends, attachedBackendEntry, backendDisplayName } from '../../stores/attachedBackends.svelte';
  import type { BackendKey } from '../../transport/backendKey';
  let computer = $state(untrack(() => getSettingsComputer() ?? selectedBackend()));
  // Track computers whose settings succeeded a fresh read during this Settings
  // open so tab switches skip redundant GetSettings() RPCs while opening
  // Settings or switching computers still refreshes once and surfaces offline
  // computers.
  const refreshedComputers = new Set<BackendKey>();
  $effect(() => {
    const target = getSettingsComputer();
    if (target !== null) computer = target;
  });

  let appVersion = $state(cachedVersion ?? '');
  $effect(() => {
    if (cachedVersion !== null) {
      appVersion = cachedVersion;
      return;
    }
    let cancelled = false;
    void loadAppVersion().then((v) => {
      if (!cancelled) appVersion = v;
    });
    return () => {
      cancelled = true;
    };
  });

  let {
    onClose,
    initialSection = DEFAULT_SETTINGS_SECTION,
  }: {
    onClose: () => void;
    initialSection?: SettingsSection;
  } = $props();

  let activeSection: SettingsSection = $state(DEFAULT_SETTINGS_SECTION);
  let needsComputer = $derived(settingsUsesComputer(activeSection));
  const namesComputer = remoteAccessAvailable();

  // Compact renders Settings as stacked screens, not the desktop two-pane
  // spread: the rail is a full-width screen and picking a section drills
  // into the page, with a back affordance in the page header. Which screen
  // is showing is the overlay store's (`isSettingsRailOpen`), so Esc and
  // the phone's back button step back to the rail before they close.
  let compact = $derived(isCompactLayout());
  let railOpen = $derived(isSettingsRailOpen());

  $effect(() => {
    activeSection = initialSection;
  });

  function openSection(section: SettingsSection): void {
    activeSection = section;
    hideSettingsRail();
  }

  let page = $derived(settingsSectionDef(activeSection));
  let loadedPageSection = $state<SettingsSection | null>(null);
  let loadedPageComponent = $state.raw<Component | null>(null);
  let pageLoadError = $state<string | null>(null);
  let pageLoadRetryNonce = $state(0);

  $effect(() => {
    const section = activeSection;
    void pageLoadRetryNonce;
    const loader = SETTINGS_PAGE_LOADERS[section];
    const cached = loader.peek();
    if (cached) {
      loadedPageSection = section;
      loadedPageComponent = cached;
      pageLoadError = null;
      return;
    }
    let cancelled = false;
    pageLoadError = null;
    void loader
      .load()
      .then((component) => {
        if (cancelled) return;
        loadedPageSection = section;
        loadedPageComponent = component;
      })
      .catch((err) => {
        if (cancelled) return;
        pageLoadError = err instanceof Error ? err.message : String(err);
      });
    return () => {
      cancelled = true;
    };
  });

  let Page = $derived(
    SETTINGS_PAGE_LOADERS[activeSection].peek() ??
      (loadedPageSection === activeSection ? loadedPageComponent : null),
  );
  let panelEl = $state<HTMLElement | null>(null);
  let selectHitSeq = 0;

  async function selectHit(hit: SettingsSearchHit): Promise<void> {
    const seq = ++selectHitSeq;
    const targetSection = hit.page.id;
    openSection(targetSection);
    if (!SETTINGS_PAGE_LOADERS[targetSection].peek()) {
      await SETTINGS_PAGE_LOADERS[targetSection].load().catch(() => null);
    }
    if (seq !== selectHitSeq || activeSection !== targetSection || !SETTINGS_PAGE_LOADERS[targetSection].peek()) {
      return;
    }
    if (hit.kind !== 'field') {
      await tick();
      if (seq !== selectHitSeq || activeSection !== targetSection) return;
      panelEl?.scrollTo({ top: 0 });
      return;
    }
    const fieldId = hit.field.id;
    await tick();
    if (seq !== selectHitSeq || activeSection !== targetSection) return;
    requestAnimationFrame(() => {
      if (seq !== selectHitSeq || activeSection !== targetSection) return;
      if (panelEl) revealSettingsField(panelEl, fieldId);
    });
  }
</script>

<div class="flex h-full flex-col bg-transparent">
  <header class="flex items-center gap-2 border-b border-border-subtle px-5 py-3 shrink-0">
    <div>
      <MicroLabel as="p">Preferences</MicroLabel>
      <h2 class="mt-1 text-sm font-semibold text-fg">Settings</h2>
    </div>
    <button
      onclick={onClose}
      class="ml-auto text-fg-subtle hover:text-fg cursor-pointer p-1 compact:p-2.5 rounded-[var(--radius-field)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors"
      aria-label="Close Settings"
    >
      <Icon icon={X} size={14} strokeWidth={2} class="opacity-90" />
    </button>
  </header>

  <div class="flex flex-1 min-h-0">
    <div class={compact && !railOpen ? 'hidden' : 'contents'}>
      <SettingsRail
        {activeSection}
        {computer}
        onSelectSection={openSection}
        onSelectHit={(hit) => void selectHit(hit)}
      />
    </div>

    <div
      bind:this={panelEl}
      class="flex-1 overflow-y-auto px-8 py-6 compact:px-4"
      class:hidden={compact && railOpen}
      role="tabpanel"
      id="settings-panel-{activeSection}"
      aria-labelledby="settings-tab-{activeSection}"
    >
      <!-- `overflow-wrap: anywhere` so an unbreakable token (a data-dir
           path in a hint, above all) wraps instead of forcing the whole
           panel to scroll sideways at phone width. It only takes effect
           where a token would otherwise overflow. -->
      <div class="mx-auto max-w-3xl [overflow-wrap:anywhere]">
        <header
          class="mb-7 flex flex-col gap-1 border-b border-border-subtle pb-5"
          data-testid="settings-page-header"
        >
          {#if compact}
            <button
              onclick={showSettingsRail}
              data-testid="settings-page-back"
              class="-ml-1.5 mb-1 flex h-9 items-center gap-1 self-start rounded-[var(--radius-field)] pr-2 text-[0.8125rem] text-fg-muted active:bg-surface-2/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
            >
              <Icon icon={ChevronLeft} size={16} strokeWidth={2} />
              All settings
            </button>
          {/if}
          <h3 class="text-[1.125rem] font-semibold tracking-tight text-fg">{page.label}</h3>
          <p class={SECTION_PROSE_CLASS}>{settingsSectionDescription(page)}</p>
          {#if needsComputer}
            <!-- A build without remote access has one computer by
                 construction, so naming it says nothing. -->
            {#if namesComputer && hasMultipleBackends()}
              <div class="mt-3 max-w-sm">
                <ComputerSelect value={computer} onchange={setSettingsComputer} />
              </div>
            {:else if namesComputer}
              {@const owner = attachedBackendEntry(computer)}
              <p class="mt-2 text-xs text-fg-subtle">Computer: {owner ? backendDisplayName(owner) : 'Unavailable'}</p>
            {/if}
          {:else if activeSection !== 'systems' && activeSection !== 'updates'}
            <p class="mt-2 text-xs text-fg-subtle">Saved on this device.</p>
          {/if}
        </header>
        {#if Page}
          {#key `${computer}:${activeSection}`}
            <ComputerSettingsPage
              backend={computer}
              {Page}
              {needsComputer}
              hasDeviceControls={activeSection === 'performance' || activeSection === 'notifications' || activeSection === 'voice'}
              alreadyRefreshed={refreshedComputers.has(computer)}
              onRefreshed={() => refreshedComputers.add(computer)}
            />
          {/key}
        {:else if pageLoadError}
          <div class="flex flex-col items-start gap-3">
            <p role="alert" class="text-sm text-error">Failed to load settings page: {pageLoadError}</p>
            <button class={PRIMARY_BUTTON_CLASS} onclick={() => { pageLoadRetryNonce += 1; }}>Retry</button>
          </div>
        {:else}
          <p role="status" class="text-sm text-fg-muted">Loading…</p>
        {/if}
      </div>
    </div>
  </div>

  <footer class="border-t border-border-subtle px-5 py-2 text-[0.6875rem] text-fg-subtle shrink-0">
    Agent Overflow{appVersion ? ` v${appVersion}` : ''}
  </footer>
</div>
