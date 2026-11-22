<script lang="ts">
  import type { ComponentProps } from 'svelte';
  import BrowserAddressBar from './BrowserAddressBar.svelte';
  import { browserShownPage, browserThreadState } from '../../stores/browserCompanion.svelte';
  import { provideBrowserPaneThread } from './browserPaneThread';

  // Test harness: the address row inside a browser pane of `threadId`, as
  // BrowserPane provides it, so the row's thread-scoped controls render.
  // With no `activePage` the row shows the thread's admitted state's shown
  // page, as BrowserPane passes it, so a state push reaches the row as it
  // does in the pane.
  type BarProps = ComponentProps<typeof BrowserAddressBar>;
  let { threadId, activePage, ...rest }: { threadId: string; activePage?: BarProps['activePage'] } & Omit<BarProps, 'activePage'> = $props();
  provideBrowserPaneThread(() => threadId);
  let shown = $derived(activePage !== undefined ? activePage : browserShownPage(browserThreadState(threadId)));
</script>

<BrowserAddressBar {...rest} activePage={shown} />
