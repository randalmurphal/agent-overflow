<script lang="ts">
  // The Settings surface: header, nav rail (SettingsRail), and the page
  // panel. Pages come from `pages.ts`; the panel renders the active page's
  // title and description from `sections.ts` above the page component so
  // no page repeats its own name.
  //
  // A search hit opens its page and, once the page has rendered, scrolls
  // to and flashes the field. The reveal runs after `tick()` (the page is
  // in the DOM) and then a frame later (layout has settled), scoped to the
  // panel so it can only ever match the page that is actually mounted.

  import { tick } from 'svelte';
  import ChevronLeft from '@lucide/svelte/icons/chevron-left';
  import X from '@lucide/svelte/icons/x';
  import Icon from '../primitives/Icon.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import MicroLabel from '../primitives/MicroLabel.svelte';
  import SettingsRail from './SettingsRail.svelte';
  import { SETTINGS_PAGES } from './pages';
  import {
    DEFAULT_SETTINGS_SECTION,
    settingsSectionDef,
    type SettingsSection,
  } from './sections';
  import { revealSettingsField, type SettingsSearchHit } from './settingsSearch';
  import { SECTION_PROSE_CLASS } from './styles';
  import { Version } from '../../stores/bindings';

  let appVersion = $state('');
  $effect(() => {
    Version()
      .then((v: string) => {
        appVersion = v;
      })
      .catch(() => {
        appVersion = '';
      });
  });

  let {
    onClose,
    initialSection = DEFAULT_SETTINGS_SECTION,
  }: {
    onClose: () => void;
    initialSection?: SettingsSection;
  } = $props();

  let activeSection: SettingsSection = $state(DEFAULT_SETTINGS_SECTION);

  // Compact renders Settings as stacked screens, not the desktop two-pane
  // spread (docs/specs/remote-access.md § The phone client): the rail is a
  // full-width screen and picking a section drills into the page, with a
  // back affordance in the page header. Desktop ignores `railOpen` — both
  // columns stay visible. A deep link to a specific section lands on that
  // page directly.
  let railOpen = $state(true);
  let compact = $derived(isCompactLayout());

  $effect(() => {
    activeSection = initialSection;
    railOpen = initialSection === DEFAULT_SETTINGS_SECTION;
  });

  function openSection(section: SettingsSection): void {
    activeSection = section;
    railOpen = false;
  }

  let page = $derived(settingsSectionDef(activeSection));
  let Page = $derived(SETTINGS_PAGES[activeSection]);
  let panelEl = $state<HTMLElement | null>(null);

  async function selectHit(hit: SettingsSearchHit): Promise<void> {
    openSection(hit.page.id);
    if (hit.kind !== 'field') {
      await tick();
      panelEl?.scrollTo({ top: 0 });
      return;
    }
    const fieldId = hit.field.id;
    await tick();
    requestAnimationFrame(() => {
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
      class="ml-auto text-fg-subtle hover:text-fg cursor-pointer p-1 rounded-[var(--radius-field)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors"
      aria-label="Close Settings"
    >
      <Icon icon={X} size={14} strokeWidth={2} class="opacity-90" />
    </button>
  </header>

  <div class="flex flex-1 min-h-0">
    <div class={compact && !railOpen ? 'hidden' : 'contents'}>
      <SettingsRail
        {activeSection}
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
      <div class="mx-auto max-w-3xl">
        <header
          class="mb-7 flex flex-col gap-1 border-b border-border-subtle pb-5"
          data-testid="settings-page-header"
        >
          {#if compact}
            <button
              onclick={() => (railOpen = true)}
              data-testid="settings-page-back"
              class="-ml-1.5 mb-1 flex h-9 items-center gap-1 self-start rounded-[var(--radius-field)] pr-2 text-[0.8125rem] text-fg-muted active:bg-surface-2/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
            >
              <Icon icon={ChevronLeft} size={16} strokeWidth={2} />
              All settings
            </button>
          {/if}
          <h3 class="text-[1.125rem] font-semibold tracking-tight text-fg">{page.label}</h3>
          <p class={SECTION_PROSE_CLASS}>{page.description}</p>
        </header>
        <Page />
      </div>
    </div>
  </div>

  <footer class="border-t border-border-subtle px-5 py-2 text-[0.6875rem] text-fg-subtle shrink-0">
    Agent Overflow{appVersion ? ` v${appVersion}` : ''}
  </footer>
</div>
