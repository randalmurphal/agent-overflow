<script lang="ts">
  // Shown under the retention field when Claude Code's own transcript
  // cleanup is shorter than the window. The backend lengthens it for the
  // Claude processes it starts (internal/provider/claude/retention.go); a
  // `claude` run outside Agent Overflow still follows the user's settings
  // file, which only the user edits.
  import { GetClaudeTranscriptRetention } from '../../stores/bindings';
  import { errString } from '../../utils/errors';
  import { settingsComputer } from './settingsComputer';

  // `days` is the retention value the page shows, saved or not.
  let { days }: { days: number } = $props();
  const { call, hasScope } = settingsComputer();

  type Retention = Awaited<ReturnType<typeof GetClaudeTranscriptRetention>>;
  // The last answer and the value it answers. It renders only while `days`
  // is still that value, so a changed window never shows the old one's note.
  let answer = $state<{ days: number; retention: Retention | null; error: string | null } | null>(null);
  let current = $derived(answer !== null && answer.days === days ? answer : null);

  $effect(() => {
    const requested = days;
    if (!hasScope('settings:read')) {
      answer = null;
      return;
    }
    let superseded = false;
    void (async () => {
      let next: NonNullable<typeof answer>;
      try {
        const retention = await call(() => GetClaudeTranscriptRetention(requested));
        next = { days: requested, retention, error: null };
      } catch (err) {
        next = { days: requested, retention: null, error: errString(err) };
      }
      if (!superseded) answer = next;
    })();
    return () => {
      superseded = true;
    };
  });
</script>

{#if current?.error}
  <p class="text-[0.75rem] text-warning" data-testid="settings-claude-retention-error">
    Could not check Claude Code's transcript retention: {current.error}
  </p>
{:else if current?.retention?.error}
  <p class="text-[0.75rem] text-warning" data-testid="settings-claude-retention-error">
    Could not read Claude Code's transcript retention, so Claude sessions started here keep Claude
    Code's own setting. {current.retention.error}
  </p>
{:else if current?.retention && current.retention.sessionDays > 0}
  {@const retention = current.retention}
  <p class="text-[0.75rem] text-fg-subtle" data-testid="settings-claude-retention-note">
    Claude Code deletes its own session transcripts after {retention.claudeDays} days{retention.explicit
      ? ''
      : ' by default'}. Claude sessions started by Agent Overflow keep them for {retention.sessionDays}
    days, but <code class="font-mono text-[0.6875rem]">claude</code> run outside Agent Overflow
    still deletes them after {retention.claudeDays}. To keep them there too, set
    <code class="font-mono text-[0.6875rem]">"cleanupPeriodDays": {retention.sessionDays}</code>
    in <code class="font-mono text-[0.6875rem]">{retention.settingsPath}</code>, and raise any
    shorter value a workspace sets in its own
    <code class="font-mono text-[0.6875rem]">.claude/settings.json</code> or
    <code class="font-mono text-[0.6875rem]">.claude/settings.local.json</code>.
  </p>
{/if}
