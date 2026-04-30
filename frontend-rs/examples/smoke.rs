// End-to-end transport smoke. Spawns the Go backend in headless mode,
// connects, calls ListProjects + ListThreads, prints the results, and
// exits. Pure integration test without GPUI in the loop — proves the
// wire shape, FNV hashing, and frame parsing match the Go side.
//
// Run from the frontend-rs/ directory:
//   AGENT_OVERFLOW_BIN=../bin/agent-overflow cargo run --example smoke
//
// Or, attaching to an already-running backend:
//   AGENT_OVERFLOW_WS_URL=ws://127.0.0.1:NNNN/ws \
//     AGENT_OVERFLOW_TOKEN=... cargo run --example smoke

use std::path::PathBuf;
use std::time::{Duration, Instant};

use agent_overflow_rs::transport::{Bootstrap, Transport, spawn_backend};
use agent_overflow_rs::{rpc};

#[tokio::main(flavor = "multi_thread", worker_threads = 2)]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info,agent_overflow_rs=debug")),
        )
        .with_target(true)
        .compact()
        .init();

    let started = Instant::now();
    let (bootstrap, _backend_guard) = match (
        std::env::var("AGENT_OVERFLOW_WS_URL"),
        std::env::var("AGENT_OVERFLOW_TOKEN"),
    ) {
        (Ok(ws_url), Ok(token)) => {
            (Bootstrap { ws_url, token }, None)
        }
        _ => {
            let bin: PathBuf = std::env::var("AGENT_OVERFLOW_BIN")
                .map(PathBuf::from)
                .unwrap_or_else(|_| {
                    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
                        .parent()
                        .unwrap()
                        .join("bin")
                        .join("agent-overflow")
                });
            tracing::info!(?bin, "spawning backend");
            let handle = spawn_backend(&bin).await?;
            let bootstrap = handle.bootstrap.clone();
            (bootstrap, Some(handle))
        }
    };

    tracing::info!(ws_url = %bootstrap.ws_url, "connecting");
    let transport = Transport::connect(&bootstrap.ws_url, &bootstrap.token)?;

    // Wait briefly for connect.
    let mut status_rx = transport.status();
    let connect_deadline = Instant::now() + Duration::from_secs(5);
    while *status_rx.borrow() != agent_overflow_rs::transport::TransportStatus::Connected {
        if Instant::now() > connect_deadline {
            anyhow::bail!("transport did not reach Connected within 5s");
        }
        if status_rx.changed().await.is_err() {
            anyhow::bail!("status channel closed");
        }
    }
    tracing::info!(elapsed_ms = started.elapsed().as_millis() as u64, "connected");

    let projects = rpc::list_projects(&transport).await?;
    tracing::info!(count = projects.len(), "ListProjects");
    for (i, p) in projects.iter().take(10).enumerate() {
        println!(
            "  project[{i}]: {} ({} threads, path={})",
            p.project.display_name(),
            p.thread_count,
            p.project.path
        );
    }

    let threads = rpc::list_threads(&transport).await?;
    tracing::info!(count = threads.len(), "ListThreads");
    for (i, t) in threads.iter().take(5).enumerate() {
        println!(
            "  thread[{i}]: {} [{}/{}] last_updated={}",
            t.display_title(),
            t.provider,
            t.model,
            t.updated_at
        );
    }

    if let Some(thread) = threads.iter().find(|t| !t.archived) {
        let paged = rpc::list_recent_thread_items(&transport, &thread.id, 5).await?;
        tracing::info!(
            thread_id = %thread.id,
            items = paged.items.len(),
            has_more = paged.has_more,
            "ListRecentThreadItems"
        );
        for item in paged.items.iter().take(5) {
            println!(
                "  item: kind={} role={} status={} summary={}",
                item.kind,
                item.role,
                item.status,
                summary_preview(&item.summary)
            );
        }
    } else {
        tracing::info!("no non-archived thread available; skipping ListRecentThreadItems");
    }

    Ok(())
}

fn summary_preview(s: &str) -> String {
    let trimmed = s.trim();
    if trimmed.len() <= 80 {
        trimmed.to_string()
    } else {
        format!("{}…", &trimmed[..80])
    }
}
