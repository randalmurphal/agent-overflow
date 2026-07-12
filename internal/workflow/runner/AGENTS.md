# internal/workflow/runner/

Pure helper functions for app-owned workflow phase execution.

## Boundaries

- Prompt interpolation, the system-owned suffix, narrative path construction,
  envelope-to-outcome mapping, and validation retry messages live here.
- Provider sessions, App state, observers, persistence, and filesystem writes
  stay in the main package.
- Inputs are already-resolved runtime workflows: phase prompts are bodies, not
  authored file paths.
- The app-owned runner provisions writing-item worktrees and setup hooks before
  the first phase, reuses persisted workspaces, and captures declared artifacts
  before reporting a successful phase completion. Cleanup remains inert until
  disposition support lands; unlanded worktrees are never discarded.
