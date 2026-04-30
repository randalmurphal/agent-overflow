// Bootstrap discovery: spawn the Go backend in headless mode and read
// the ws_url + token off its stdout sentinel.
//
// The launcher's `--print-url-fd 0` flag (see /main.go runHeadless)
// tells the backend to:
//   1. Bind the transport on an ephemeral loopback port.
//   2. Generate a fresh token.
//   3. Emit `__AO_BOOTSTRAP__: {"wsUrl":"ws://127.0.0.1:<port>/ws","token":"..."}\n`
//      on stdout.
//   4. Stay running until SIGINT/SIGTERM.
//
// We spawn that, parse the sentinel, then return both the bootstrap
// payload and a process handle so the caller can shut the backend down
// on app exit. Subsequent stdout lines (Go-side `log.Printf`) are
// forwarded to our tracing layer at INFO so a misbehaving backend
// surfaces in our own logs without a separate window.

use std::process::Stdio;

use anyhow::{Context, Result, anyhow, bail};
use serde::Deserialize;
use tokio::io::{AsyncBufReadExt, BufReader};
use tokio::process::{Child, Command};

const SENTINEL: &str = "__AO_BOOTSTRAP__:";
/// How long we'll wait for the backend to print its bootstrap line
/// before declaring the spawn dead. The Go boot path is mostly SQLite
/// schema migrations + dispatcher reflection; on a cold disk this can
/// take a couple seconds, but never minutes. 30s is a generous ceiling.
const BOOTSTRAP_TIMEOUT_SECS: u64 = 30;

#[derive(Debug, Clone)]
pub struct Bootstrap {
    pub ws_url: String,
    pub token: String,
}

/// Stdout-sentinel bootstrap shape. The headless backend
/// (--print-url-fd 0) emits `{"port":N, "token":"..."}`; the HTTP
/// `/bootstrap.json` endpoint emits `{"wsUrl":"...", "token":"..."}`.
/// We accept either and normalize.
#[derive(Debug, Deserialize)]
struct RawBootstrap {
    #[serde(default, rename = "wsUrl")]
    ws_url: Option<String>,
    #[serde(default)]
    port: Option<u16>,
    token: String,
}

impl Bootstrap {
    fn from_raw(raw: RawBootstrap) -> anyhow::Result<Self> {
        let ws_url = match (raw.ws_url, raw.port) {
            (Some(url), _) => url,
            (None, Some(port)) => format!("ws://127.0.0.1:{port}/ws"),
            (None, None) => {
                anyhow::bail!("bootstrap missing both wsUrl and port")
            }
        };
        Ok(Self {
            ws_url,
            token: raw.token,
        })
    }
}

/// Owns the spawned backend process. Dropping it sends SIGKILL via
/// tokio's Child Drop semantics; callers that care about clean shutdown
/// should call `shutdown()` explicitly first.
pub struct BootstrapHandle {
    pub bootstrap: Bootstrap,
    child: Child,
}

impl BootstrapHandle {
    pub async fn shutdown(mut self) -> Result<()> {
        // SIGTERM via kill_on_drop fires when the handle drops; for an
        // explicit shutdown we'd want SIGINT first to let the backend's
        // graceful path run. tokio's Child only exposes `kill` (SIGKILL
        // on Unix); the headless backend installs a signal handler that
        // shuts the transport down on SIGINT/SIGTERM. We send SIGTERM
        // via libc and fall back to kill on timeout.
        #[cfg(unix)]
        if let Some(pid) = self.child.id() {
            // SAFETY: pid comes from a process we spawned and still own;
            // libc::kill on a known-our pid is well-defined. SIGTERM is
            // the documented graceful signal — the headless backend
            // installs a SIGTERM handler in main.go runHeadless.
            let _ = unsafe { libc::kill(pid as libc::pid_t, libc::SIGTERM) };
        }

        // 5s grace, then escalate.
        let waited = tokio::time::timeout(
            std::time::Duration::from_secs(5),
            self.child.wait(),
        )
        .await;
        match waited {
            Ok(Ok(_)) => Ok(()),
            Ok(Err(e)) => Err(e.into()),
            Err(_) => {
                let _ = self.child.kill().await;
                Ok(())
            }
        }
    }
}

/// Spawns `agent-overflow --print-url-fd 0 --listen 127.0.0.1:0`
/// (or the equivalent set on this platform) and returns once the
/// bootstrap line is parsed. Forwards subsequent stdout/stderr to
/// tracing.
///
/// `binary` is the path to the agent-overflow executable. We don't
/// hardcode it — the spike's main accepts an env var override or
/// defaults to the project's build artifact in `bin/`.
pub async fn spawn_backend(binary: &std::path::Path) -> Result<BootstrapHandle> {
    if !binary.exists() {
        bail!(
            "agent-overflow binary not found at {} — build it with `make go-build` first",
            binary.display()
        );
    }

    let mut cmd = Command::new(binary);
    cmd.arg("--print-url-fd")
        .arg("0")
        .arg("--listen")
        .arg("127.0.0.1:0")
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .stdin(Stdio::null())
        .kill_on_drop(true);

    let mut child = cmd.spawn().with_context(|| {
        format!("spawn agent-overflow at {}", binary.display())
    })?;

    let stdout = child
        .stdout
        .take()
        .ok_or_else(|| anyhow!("child stdout not captured"))?;
    let stderr = child
        .stderr
        .take()
        .ok_or_else(|| anyhow!("child stderr not captured"))?;

    let bootstrap = tokio::time::timeout(
        std::time::Duration::from_secs(BOOTSTRAP_TIMEOUT_SECS),
        read_bootstrap(stdout),
    )
    .await
    .map_err(|_| anyhow!("backend did not emit bootstrap within {BOOTSTRAP_TIMEOUT_SECS}s"))??;

    // After bootstrap, drain stdout/stderr into tracing so the backend's
    // log.Printf shows up in our journal without a separate window.
    if let Some(rest) = bootstrap.tail {
        tokio::spawn(async move {
            let mut reader = BufReader::new(rest);
            let mut buf = String::new();
            loop {
                buf.clear();
                match reader.read_line(&mut buf).await {
                    Ok(0) => break,
                    Ok(_) => tracing::info!(target: "backend.stdout", "{}", buf.trim_end()),
                    Err(e) => {
                        tracing::warn!(target: "backend.stdout", "read error: {e}");
                        break;
                    }
                }
            }
        });
    }

    tokio::spawn(async move {
        let mut reader = BufReader::new(stderr);
        let mut buf = String::new();
        loop {
            buf.clear();
            match reader.read_line(&mut buf).await {
                Ok(0) => break,
                Ok(_) => tracing::warn!(target: "backend.stderr", "{}", buf.trim_end()),
                Err(e) => {
                    tracing::warn!(target: "backend.stderr", "read error: {e}");
                    break;
                }
            }
        }
    });

    Ok(BootstrapHandle {
        bootstrap: bootstrap.payload,
        child,
    })
}

struct ParsedBootstrap {
    payload: Bootstrap,
    tail: Option<tokio::process::ChildStdout>,
}

/// Reads stdout line-by-line until it hits the sentinel or EOF.
/// Returns the parsed bootstrap and (because BufReader consumed the
/// underlying ChildStdout) returns None for the remaining stream — the
/// caller's drain task receives a None tail and just skips.
async fn read_bootstrap(stdout: tokio::process::ChildStdout) -> Result<ParsedBootstrap> {
    let mut reader = BufReader::new(stdout);
    let mut line = String::new();
    loop {
        line.clear();
        let n = reader.read_line(&mut line).await.context("read stdout")?;
        if n == 0 {
            bail!("backend exited before emitting bootstrap");
        }
        let trimmed = line.trim();
        if let Some(rest) = trimmed.strip_prefix(SENTINEL) {
            let json = rest.trim_start();
            let raw: RawBootstrap = serde_json::from_str(json)
                .with_context(|| format!("parse bootstrap JSON: {json}"))?;
            let payload = Bootstrap::from_raw(raw)?;
            // Reader still owns the underlying stream (BufReader
            // consumed bytes through the sentinel line). Hand it back
            // wrapped so the drain task can pick up where we left off.
            return Ok(ParsedBootstrap {
                payload,
                tail: Some(reader.into_inner()),
            });
        } else if !trimmed.is_empty() {
            tracing::debug!(target: "backend.stdout", "{}", trimmed);
        }
    }
}
