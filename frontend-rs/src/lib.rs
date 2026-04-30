// Library facade for the spike. The binary at src/main.rs is just the
// GPUI entry point; everything else lives here so examples / future
// integration tests can import the same modules.

pub mod app;
pub mod models;
pub mod rpc;
pub mod theme;
pub mod transport;
pub mod ui;
