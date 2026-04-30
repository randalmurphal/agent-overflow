// Entry point for the GPUI frontend spike.
//
// Boots a tokio runtime on a worker thread (the transport layer is
// async-by-tokio, GPUI runs on its own foreground loop), connects to the
// Go backend, and hands the resulting `AppState` to the GPUI window so
// views read live data from real RPCs and event channels.

use std::sync::Arc;

use agent_overflow_rs::{app, ui};
use anyhow::{Context, Result};
use gpui::{App, AppContext, Application, Bounds, WindowBounds, WindowOptions, px, size};

fn main() -> Result<()> {
    init_tracing();

    // Two-runtime layout: tokio for transport, GPUI for UI. We hand the
    // tokio handle into AppState so async tasks scheduled from views land
    // on the right reactor.
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .thread_name("ao-tokio")
        .build()
        .context("build tokio runtime")?;
    let handle = runtime.handle().clone();

    // Keep the runtime alive for the lifetime of the process. We don't
    // shut it down on window close because gpui drives shutdown via
    // App::run; the runtime drops with the process.
    Box::leak(Box::new(runtime));

    Application::new().run(move |cx: &mut App| {
        let state = Arc::new(app::AppState::spawn(handle.clone(), cx));

        let bounds = Bounds::centered(None, size(px(1280.), px(800.)), cx);
        cx.open_window(
            WindowOptions {
                window_bounds: Some(WindowBounds::Windowed(bounds)),
                titlebar: Some(gpui::TitlebarOptions {
                    title: Some("Agent Overflow".into()),
                    ..Default::default()
                }),
                ..Default::default()
            },
            |_, cx| cx.new(|cx| ui::root::Root::new(state.clone(), cx)),
        )
        .expect("open initial window");

        cx.activate(true);
    });

    Ok(())
}

fn init_tracing() {
    let filter = tracing_subscriber::EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info,agent_overflow_rs=debug"));
    tracing_subscriber::fmt()
        .with_env_filter(filter)
        .with_target(true)
        .with_writer(std::io::stderr)
        .compact()
        .init();
}
