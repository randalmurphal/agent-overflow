// Domain types used by the UI. Names + shapes mirror the generated TS
// bindings (frontend/bindings/agent-overflow/internal/store/models.ts)
// because the wire is JSON and the field names ARE the contract.
//
// We deliberately model only the fields the spike's views actually
// consume. The wire envelope is `serde_json::Value` everywhere else, so
// adding a new field is just adding a serde struct field — no codegen,
// no schema migration.

pub mod item;
pub mod project;
pub mod thread;

pub use item::{Item, ItemLane, PagedItems};
pub use project::ProjectWithCounts;
pub use thread::Thread;
