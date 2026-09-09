# `internal/keybindings`

Loads, validates, and persists user keybinding overrides over generated
defaults. `internal/uikeys` is a separate native-window shortcut map and does
not define these command ids.

- Loading distinguishes an absent file from an unreadable or invalid file and
  returns the load error alongside merged defaults.
- The empty binding explicitly means unbound and must survive save/load round trips.
- The backend validates required fields and list bounds, then persists key and
  `when` strings verbatim. The frontend owns chord and context-expression
  syntax validation.
- Accelerator projection parses only the small native menu vocabulary; it is
  not the frontend keybinding compiler.
- Update the generator input, generated output, frontend mirror, and completeness tests together.

UI conflict policy and event handling remain frontend responsibilities.
