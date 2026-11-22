<script lang="ts">
  // A project's list as its entry renders it: the limit read from the
  // record the entry key names, as buildSidebarEntries reads it for an entry
  // whose key has not moved.
  import type { ComponentProps } from 'svelte';
  import ProjectThreadList from '../../lib/components/sidebar/ProjectThreadList.svelte';
  import { entryRecordForTest } from '../../lib/stores/sidebarEntryContinuity.svelte';

  let props: Omit<ComponentProps<typeof ProjectThreadList>, 'listLimit'> = $props();
</script>

<ProjectThreadList {...props} listLimit={entryRecordForTest(props.listKey).listLimit} />
