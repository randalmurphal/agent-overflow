<script lang="ts">
  // What an agent did, "3 tools · 18.2k tokens": a background subagent's
  // row (BackgroundTaskTrayRow) and a sub-thread's (AgentTrayRow) show the
  // same counts the same way. Either count is left out while unknown. For a
  // sub-thread (docs/specs/standing-agents.md, "Agents segment"): tools is
  // each top-level tool call the thread executed, once, none a fork, import
  // or restore copied; tokens is what it spent itself (fresh input, cache
  // writes and output; cache reads are no spend), as a subagent's row
  // counts its own, none of its native agents' or the threads it spawned.
  import { formatAgentTokens, formatToolUses } from '../../utils/subagentProgress';

  interface Props {
    toolUses: number | null;
    tokens: number | null;
    /** Prefix of the counts' test ids, the row's own. */
    testIdPrefix: string;
  }

  let { toolUses, tokens, testIdPrefix }: Props = $props();

  let toolsLabel = $derived(formatToolUses(toolUses));
  let tokensLabel = $derived(formatAgentTokens(tokens));
</script>

{#if toolsLabel}
  <span class="shrink-0" data-testid={`${testIdPrefix}-tools`}>{toolsLabel}</span>
{/if}
{#if toolsLabel && tokensLabel}<span aria-hidden="true">·</span>{/if}
{#if tokensLabel}
  <span class="shrink-0" data-testid={`${testIdPrefix}-tokens`}>{tokensLabel}</span>
{/if}
