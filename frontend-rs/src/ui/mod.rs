// View tree.
//
//   root
//     ├── connection_pill (top-right, transient)
//     ├── sidebar (left)
//     │     └── thread list (sectioned by project)
//     └── main pane (right)
//           ├── header (title + provider + model)
//           ├── timeline (scrollable list of items)
//           └── composer placeholder
//
// All views are GPUI Entities with `impl Render`. State they observe
// lives on `AppState` (Entity<*Model>); each view subscribes to the
// model entities it cares about so cx.notify() on the model triggers a
// re-render here.

pub mod root;
pub mod sidebar;
pub mod status_bar;
pub mod timeline;
