# Advanced Features + Parity Loop — Progress Tracker

## Status: NOT STARTED

## Codebase Patterns

(Populated as iterations discover important patterns.)

## Known Issues

(Issues found during review phase or parity audit. Highest severity first. Agent fixes these BEFORE doing scheduled work items.)

### From Parity Audit (pre-populated)

- CRITICAL: No proposed plan rendering or handling (forge has ProposedPlanCard with expandable markdown, copy, save, download)
- CRITICAL: No thread forking support (forge forkThread creates new session with resumeCursor)
- CRITICAL: No AI-generated worktree branch names (forge generates descriptive names on first user message)
- CRITICAL: No AI-generated thread titles for Claude sessions (Codex sends thread/name/updated but Claude doesn't — forge generates titles for both)
- CRITICAL: No session restart for model changes mid-conversation (forge restarts session with new model, carries conversation via resumeCursor)
- IMPORTANT: No inline diff upgrade from summary to exact patch (forge extracts filtered unified diffs from turn checkpoint)
- IMPORTANT: No command inline diff capture for mv/rm/cp (forge captures pre-command state)
- IMPORTANT: No stacked/compound git actions (forge has commit+push+pr compound flows)
- IMPORTANT: No PR-based thread creation (forge can take a PR URL, fetch branch, set up workspace)
- IMPORTANT: No settings config issue surfacing to UI (forge tracks and displays malformed config errors)
- IMPORTANT: No mid-turn guard when user sends message while turn is active

## Resolved Issues

(Issues moved here after being fixed and committed.)

## Completed Work Items

(None yet.)

## Iteration Log

(Entries added after each commit.)

## Review Log

(Entries added during review phase.)
