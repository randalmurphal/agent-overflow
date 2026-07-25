# Refactoring Principles

Use these rules for multi-session cleanup work. The goal is to make the
codebase easier to extend without changing product behavior accidentally.

## Preserve Behavior First

Refactor slices should start with the smallest observable surface:
same package, same public APIs, same wire shapes, same database schema.
Move code behind existing tests before changing contracts. If behavior
must change, make that its own explicit feature or bug-fix slice.

## Split By Ownership

Create files and packages around a single responsibility:

- provider adapters parse provider wire protocol and emit normalized
  `provider.ProviderEvent` values.
- triage decides persistence and frontend routing.
- app-level code coordinates cross-boundary workflows such as session
  startup, sends, queues, forks, and design setup.
- frontend stores own visible UI state and projection, not backend
  business decisions.

Do not split only to reduce line count. Split when a future change has
an obvious home after the move.

## Keep Boundaries Inward

Provider packages must not write SQLite or emit UI events directly.
Triage must not inspect provider-native JSON except through normalized
event meta that the provider package deliberately surfaced. Frontend
code must call through typed bindings and transport stores rather than
reaching around the app boundary.

## Prefer Same-Package Extraction First

For large, risky files, first extract same-package files with unexported
helpers intact. Promote to a new package only after the dependency
direction and public API are obvious. Avoid inventing a generic
abstraction until at least two concrete callers need the same contract.

## Tests Move With Behavior

Every extraction keeps existing tests passing. Add focused tests when
the extracted concern was previously under-covered, especially for
bounded buffers, lifecycle dedupe, event ordering, redaction, and error
drain paths. Run the smallest package test after each slice and the
project-required checks before declaring the session complete.
