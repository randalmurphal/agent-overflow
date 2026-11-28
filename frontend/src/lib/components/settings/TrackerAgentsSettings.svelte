<script lang="ts">
  // Settings > Issues > Agents: what a thread in the project may do to
  // its tickets through the tracker tools (docs/specs/issues.md, "Setup").
  // Reading is always on; Change tickets and Ask before changing several
  // at once are the project's setup's agents section, each on until
  // turned off, which the computer's tracker tools obey for every thread
  // of the project (trackerapp.AgentRules).
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { agentRuleOn, withAgentRule, type AgentRule } from '../../stores/trackerSetup.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import TrackerProjectSettings, { type TrackerProjectPage } from './TrackerProjectSettings.svelte';

  const ROWS: readonly { rule: AgentRule | null; title: string; text: string }[] = [
    { rule: null, title: 'Read tickets', text: 'Every thread can read tickets, comments and history.' },
    { rule: 'change_tickets', title: 'Change tickets', text: 'Threads that are not read-only can create, edit, move and comment on tickets, and log your time once you approve it.' },
    { rule: 'ask_several', title: 'Ask before changing several at once', text: 'A change to more than one ticket waits for your approval in its thread.' },
  ];

  const compact = $derived(isCompactLayout());
</script>

<TrackerProjectSettings
  page="agents"
  projectField="tracker-agents.project"
  bodyField="tracker-agents.rules"
  heading="What agents may do"
  hint="Choose the project whose threads these apply to."
  what="Agents settings"
  section="agents"
  reading="Reading the settings…"
>
  {#snippet body(page: TrackerProjectPage)}
    <ul class="flex flex-col gap-3">
      {#each ROWS as row (row.title)}
        {@const on = row.rule === null || agentRuleOn(page.setup, row.rule)}
        <li class="flex min-w-0 items-center gap-3 {compact ? 'min-h-9' : ''}" data-testid="tracker-agent-rule" data-rule={row.rule ?? 'read'}>
          <div class="flex min-w-0 flex-1 flex-col">
            <span class="text-[0.8125rem] text-fg">{row.title}</span>
            <span class="text-[0.71875rem] text-fg-hint">{row.text}</span>
          </div>
          <ToggleSwitch
            checked={on}
            ariaLabel={row.title}
            disabled={row.rule === null || page.lockReason !== ''}
            title={row.rule === null ? 'Always on' : page.lockReason || undefined}
            onToggle={(next) => {
              const rule = row.rule;
              if (rule) void page.save((held) => withAgentRule(held, rule, next));
            }}
          />
        </li>
      {/each}
    </ul>
    <p class="text-[0.71875rem] text-fg-hint">Every change an agent makes shows in its thread and is marked as the agent's in the ticket's history.</p>
  {/snippet}
</TrackerProjectSettings>
