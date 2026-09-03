# components/settings/

The Settings overlay: a nav rail of pages, one page component per topic,
and a search box over every control.

## Adding or moving a control

Three files describe a control, and a test ties them together.

- `sections.ts` is the page list: id, rail label, group, and the one-line
  description `SettingsView` renders above the page. A page component never
  renders its own title. In-page sub-sections use `SettingsHeader` with a
  sentence-case title, and a page that is a run of sections wraps them in
  `.settings-sections` (app.css), which rules and spaces between siblings;
  no section carries its own top margin or border.
- `pages.ts` maps each page id to its component; the record is total over
  `SettingsSection`, so a page without a component does not compile.
- `fields.ts` is the search index. Every `SettingsField` takes a required
  `id: SettingsFieldId` from it, so a control cannot be added without
  becoming searchable and a typo is a compile error. A block that is not a
  labelled row (a radio group, the accounts list, the auto-compact sliders)
  stamps `data-settings-field` and `data-settings-label` on its root itself.
  Shared provider controls use `providerFieldId(provider, slug)`;
  provider-specific ones are static entries under that provider's page.

`fields.test.ts` mounts every page with the shipped defaults and fails on a
registered id that does not render, a rendered anchor that is not
registered, or a label or hint that differs from the index. Mark an entry
`conditional` only when the default fixture genuinely cannot show it (a
platform-gated block, a control behind another setting). The index's
`label` and `hint` are canonical: change the row's copy and the index in
the same edit.

`keywords` carry what neither label nor hint says ("sleep" for the
keep-awake toggle). Search matches every query token against label, hint,
keywords, in-page heading, page label and group, and ranks label hits
first (`settingsSearch.ts`). A hit opens the page and `revealSettingsField`
scrolls to and flashes the anchor.

## Deep links

`openSettingsOverlay(section)` takes a page id. A provider-scoped caller
routes through `providerSettingsSection(providerId)`, which lands a
dependent provider (Claude TUI) on its parent's page and throws for a
provider with no page rather than guessing.

## Provider pages

`ProviderSettingsPage.svelte` renders one provider's whole page (setup,
environment, accounts, context window, system prompt, tools, and the
Claude-only session sections). `ClaudeSettings` / `CodexSettings` are the
two instantiations. Every section renders regardless of the Enabled
toggle; there is no "enable it first" gate.
