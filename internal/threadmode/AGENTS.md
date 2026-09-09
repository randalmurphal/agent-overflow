# Thread mode validation

This package validates the independent interaction-mode and provider
runtime-mode axes. Persistence, events, and active-session restarts belong to
`internal/app`.

## Contracts

- Public create and update validators accept only manually selectable
  interaction modes. Discussion and workflow modes are saga-owned because their
  creation requires corresponding channel or run state.
- Keep saga-owned modes legal for persistence and hidden from ordinary thread
  listings and pickers. Do not make them publicly creatable to simplify a
  caller.
- `ValidateCreate` may default an empty value to `DefaultCreateMode`.
  `ValidateSet` represents an explicit user action and rejects empty input.
- Derive runtime-mode membership from `provider.AllRuntimeModes`; do not keep a
  second local list.
- `HiddenModes` must return a stable copy so callers cannot mutate package
  policy.
- Validators remain pure and normalize surrounding whitespace before checking
  membership.
