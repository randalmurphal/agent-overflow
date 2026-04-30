// AppState owns the GPUI-side mirrors of backend data plus the bridge
// that pumps `UiUpdate` messages from tokio onto the foreground.
//
// Threading model:
//   - Tokio side owns the transport client, the bootstrap subprocess,
//     and every RPC future. Async work that touches the network always
//     happens here.
//   - GPUI side owns the `Entity<*Model>` instances and a foreground
//     async loop that drains an mpsc::UnboundedReceiver<UiUpdate>. Each
//     message mutates one entity and calls cx.notify().
//   - UI commands (sidebar click, retry button) hand a Handle into a
//     small spawn helper that calls into tokio directly; the resulting
//     UiUpdate flows back through the same one-way pipe.
//
// We deliberately do NOT cross-thread `AsyncApp` — GPUI's foreground
// context is thread-bound by design. Bridging by message keeps that
// guarantee while still letting the transport run on tokio.

use std::path::PathBuf;
use std::sync::Arc;

use anyhow::Context as _;
use gpui::{App, AppContext, AsyncApp, Entity};
use parking_lot::Mutex;
use tokio::runtime::Handle;
use tokio::sync::mpsc::{self, UnboundedReceiver, UnboundedSender};

use crate::models::{Item, PagedItems, ProjectWithCounts, Thread};
use crate::rpc;
use crate::transport::{
    Bootstrap, BootstrapHandle, Transport, TransportStatus, spawn_backend,
};

/// Messages produced by tokio-side tasks and consumed by GPUI's
/// foreground drain loop. Each variant maps 1:1 onto an entity update.
pub enum UiUpdate {
    TransportStatus(TransportStatus),
    Projects(Result<Vec<ProjectWithCounts>, String>),
    Threads(Result<Vec<Thread>, String>),
    SelectThread(Option<String>),
    Timeline {
        thread_id: String,
        result: Result<PagedItems, String>,
    },
    Bootstrap(Result<(), String>),
}

pub struct AppState {
    pub tokio: Handle,
    pub theme: crate::theme::Theme,

    /// Set once after connect lands. Behind a Mutex so the bootstrap
    /// task can install it without coordinating Cell lifetimes.
    transport: Arc<Mutex<Option<Transport>>>,
    /// Backend process handle. Held so SIGTERM fires when AppState
    /// drops at the end of the program.
    _backend: Arc<Mutex<Option<BootstrapHandle>>>,

    /// One-way pipe: tokio → GPUI foreground.
    updates_tx: UnboundedSender<UiUpdate>,

    pub projects: Entity<ProjectsModel>,
    pub threads: Entity<ThreadsModel>,
    pub selection: Entity<SelectionModel>,
    pub timeline: Entity<TimelineModel>,
    pub status: Entity<StatusModel>,
}

#[derive(Default)]
pub struct SelectionModel {
    pub project_id: Option<String>,
    pub thread_id: Option<String>,
}

#[derive(Default)]
pub struct ProjectsModel {
    pub list: Vec<ProjectWithCounts>,
    pub error: Option<String>,
}

#[derive(Default)]
pub struct ThreadsModel {
    pub list: Vec<Thread>,
    pub error: Option<String>,
}

#[derive(Default)]
pub struct TimelineModel {
    pub thread_id: Option<String>,
    pub items: Vec<Item>,
    pub has_more: bool,
    pub error: Option<String>,
}

pub struct StatusModel {
    pub transport: TransportStatus,
    pub bootstrap_error: Option<String>,
}

impl Default for StatusModel {
    fn default() -> Self {
        Self {
            transport: TransportStatus::Disconnected,
            bootstrap_error: None,
        }
    }
}

impl AppState {
    /// Build the state, kick off the bootstrap, and wire the foreground
    /// drain loop. Returns immediately — views show a "connecting…"
    /// placeholder until the bootstrap task completes.
    pub fn spawn(tokio: Handle, cx: &mut App) -> Self {
        let transport_slot = Arc::new(Mutex::new(None));
        let backend_slot = Arc::new(Mutex::new(None));

        let projects = cx.new(|_| ProjectsModel::default());
        let threads = cx.new(|_| ThreadsModel::default());
        let selection = cx.new(|_| SelectionModel::default());
        let timeline = cx.new(|_| TimelineModel::default());
        let status = cx.new(|_| StatusModel::default());

        let (updates_tx, updates_rx) = mpsc::unbounded_channel::<UiUpdate>();

        // Tokio side: bootstrap + initial RPCs. Owns the transport.
        tokio.spawn(bootstrap_and_load(
            tokio.clone(),
            transport_slot.clone(),
            backend_slot.clone(),
            updates_tx.clone(),
        ));

        // GPUI side: drain the queue and apply each message.
        let drain_projects = projects.clone();
        let drain_threads = threads.clone();
        let drain_selection = selection.clone();
        let drain_timeline = timeline.clone();
        let drain_status = status.clone();
        cx.spawn(async move |async_cx: &mut AsyncApp| {
            drain_updates(
                updates_rx,
                async_cx.clone(),
                drain_projects,
                drain_threads,
                drain_selection,
                drain_timeline,
                drain_status,
            )
            .await;
        })
        .detach();

        Self {
            tokio,
            theme: crate::theme::Theme::dark(),
            transport: transport_slot,
            _backend: backend_slot,
            updates_tx,
            projects,
            threads,
            selection,
            timeline,
            status,
        }
    }

    pub fn transport(&self) -> Option<Transport> {
        self.transport.lock().clone()
    }

    pub fn updates_tx(&self) -> UnboundedSender<UiUpdate> {
        self.updates_tx.clone()
    }
}

async fn drain_updates(
    mut rx: UnboundedReceiver<UiUpdate>,
    cx: AsyncApp,
    projects: Entity<ProjectsModel>,
    threads: Entity<ThreadsModel>,
    selection: Entity<SelectionModel>,
    timeline: Entity<TimelineModel>,
    status: Entity<StatusModel>,
) {
    while let Some(msg) = rx.recv().await {
        let result = cx.update(|cx| match msg {
            UiUpdate::TransportStatus(s) => {
                status.update(cx, |m, cx| {
                    m.transport = s;
                    cx.notify();
                });
            }
            UiUpdate::Projects(Ok(list)) => {
                projects.update(cx, |m, cx| {
                    m.list = list;
                    m.error = None;
                    cx.notify();
                });
            }
            UiUpdate::Projects(Err(msg)) => {
                projects.update(cx, |m, cx| {
                    m.error = Some(msg);
                    cx.notify();
                });
            }
            UiUpdate::Threads(Ok(list)) => {
                threads.update(cx, |m, cx| {
                    m.list = list;
                    m.error = None;
                    cx.notify();
                });
            }
            UiUpdate::Threads(Err(msg)) => {
                threads.update(cx, |m, cx| {
                    m.error = Some(msg);
                    cx.notify();
                });
            }
            UiUpdate::SelectThread(thread_id) => {
                selection.update(cx, |m, cx| {
                    m.thread_id = thread_id.clone();
                    cx.notify();
                });
                if let Some(id) = thread_id {
                    timeline.update(cx, |m, cx| {
                        m.thread_id = Some(id);
                        m.items.clear();
                        m.error = None;
                        cx.notify();
                    });
                } else {
                    timeline.update(cx, |m, cx| {
                        m.thread_id = None;
                        m.items.clear();
                        cx.notify();
                    });
                }
            }
            UiUpdate::Timeline { thread_id, result } => {
                timeline.update(cx, |m, cx| {
                    if m.thread_id.as_deref() != Some(thread_id.as_str()) {
                        // User has switched threads since this load was
                        // dispatched; drop the result so we don't paint
                        // stale items into the new selection.
                        return;
                    }
                    match result {
                        Ok(paged) => {
                            m.items = paged.items;
                            m.has_more = paged.has_more;
                            m.error = None;
                        }
                        Err(msg) => {
                            m.error = Some(msg);
                        }
                    }
                    cx.notify();
                });
            }
            UiUpdate::Bootstrap(result) => {
                status.update(cx, |m, cx| {
                    m.bootstrap_error = result.err();
                    cx.notify();
                });
            }
        });
        if result.is_err() {
            // App is shutting down; the foreground is gone.
            break;
        }
    }
}

async fn bootstrap_and_load(
    handle: Handle,
    transport_slot: Arc<Mutex<Option<Transport>>>,
    backend_slot: Arc<Mutex<Option<BootstrapHandle>>>,
    updates: UnboundedSender<UiUpdate>,
) {
    let bootstrap = match resolve_bootstrap(&backend_slot).await {
        Ok(b) => b,
        Err(e) => {
            let msg = format!("{e:#}");
            tracing::error!(error = %msg, "bootstrap unavailable");
            let _ = updates.send(UiUpdate::Bootstrap(Err(msg.clone())));
            let _ = updates.send(UiUpdate::Projects(Err(msg.clone())));
            let _ = updates.send(UiUpdate::Threads(Err(msg)));
            return;
        }
    };

    tracing::info!(ws_url = %bootstrap.ws_url, "connecting transport");
    let transport = match Transport::connect(&bootstrap.ws_url, &bootstrap.token) {
        Ok(t) => t,
        Err(e) => {
            let msg = format!("{e}");
            let _ = updates.send(UiUpdate::Bootstrap(Err(msg)));
            return;
        }
    };
    *transport_slot.lock() = Some(transport.clone());
    let _ = updates.send(UiUpdate::Bootstrap(Ok(())));

    // Bridge transport status onto the UI message bus.
    {
        let updates = updates.clone();
        let mut status_rx = transport.status();
        handle.spawn(async move {
            loop {
                let snapshot = *status_rx.borrow_and_update();
                if updates
                    .send(UiUpdate::TransportStatus(snapshot))
                    .is_err()
                {
                    break;
                }
                if status_rx.changed().await.is_err() {
                    break;
                }
            }
        });
    }

    // Cold-start RPCs in parallel. Errors per-list so a single failure
    // doesn't blank both panels.
    let t1 = transport.clone();
    let updates_p = updates.clone();
    handle.spawn(async move {
        let result = rpc::list_projects(&t1).await.map_err(|e| e.to_string());
        let _ = updates_p.send(UiUpdate::Projects(result));
    });

    let t2 = transport.clone();
    let updates_t = updates.clone();
    let handle_inner = handle.clone();
    handle.spawn(async move {
        match rpc::list_threads(&t2).await {
            Ok(mut list) => {
                list.sort_by(|a, b| {
                    b.pinned_at
                        .cmp(&a.pinned_at)
                        .then_with(|| b.updated_at.cmp(&a.updated_at))
                });
                let auto_select = list.first().map(|t| t.id.clone());
                let _ = updates_t.send(UiUpdate::Threads(Ok(list)));
                if let Some(id) = auto_select {
                    let _ = updates_t.send(UiUpdate::SelectThread(Some(id.clone())));
                    spawn_timeline_load(&handle_inner, t2, id, updates_t);
                }
            }
            Err(e) => {
                let _ = updates_t.send(UiUpdate::Threads(Err(e.to_string())));
            }
        }
    });
}

fn spawn_timeline_load(
    handle: &Handle,
    transport: Transport,
    thread_id: String,
    updates: UnboundedSender<UiUpdate>,
) {
    handle.spawn(async move {
        let result = rpc::list_recent_thread_items(&transport, &thread_id, 50)
            .await
            .map_err(|e| e.to_string());
        let _ = updates.send(UiUpdate::Timeline { thread_id, result });
    });
}

/// Either spawn the backend ourselves OR attach to a pre-running one
/// via `AGENT_OVERFLOW_WS_URL` + `AGENT_OVERFLOW_TOKEN` env vars. The
/// env-var path is the simplest dev loop: run `make dev` separately and
/// point the spike at the resulting transport.
async fn resolve_bootstrap(
    backend_slot: &Arc<Mutex<Option<BootstrapHandle>>>,
) -> anyhow::Result<Bootstrap> {
    if let (Ok(ws_url), Ok(token)) = (
        std::env::var("AGENT_OVERFLOW_WS_URL"),
        std::env::var("AGENT_OVERFLOW_TOKEN"),
    ) {
        return Ok(Bootstrap { ws_url, token });
    }

    let bin_env = std::env::var("AGENT_OVERFLOW_BIN").ok();
    let bin = match bin_env {
        Some(p) => PathBuf::from(p),
        None => default_backend_binary().context("locate agent-overflow binary")?,
    };
    let handle = spawn_backend(&bin).await?;
    let bootstrap = handle.bootstrap.clone();
    *backend_slot.lock() = Some(handle);
    Ok(bootstrap)
}

fn default_backend_binary() -> anyhow::Result<PathBuf> {
    let exe = std::env::current_exe()?;
    if let Some(parent) = exe.parent() {
        let candidate = parent.join("agent-overflow");
        if candidate.exists() {
            return Ok(candidate);
        }
    }
    let cwd = std::env::current_dir()?;
    let candidate = cwd.join("agent-overflow");
    if candidate.exists() {
        return Ok(candidate);
    }
    anyhow::bail!(
        "agent-overflow binary not found; set AGENT_OVERFLOW_BIN or run from repo root"
    )
}

/// Click handler entry: switch active thread, emit a SelectThread
/// update, and kick off a timeline reload on tokio.
pub fn select_thread(state: &Arc<AppState>, thread_id: String) {
    let _ = state
        .updates_tx
        .send(UiUpdate::SelectThread(Some(thread_id.clone())));
    if let Some(transport) = state.transport() {
        spawn_timeline_load(
            &state.tokio,
            transport,
            thread_id,
            state.updates_tx.clone(),
        );
    } else {
        tracing::debug!("select_thread fired before transport ready");
    }
}

