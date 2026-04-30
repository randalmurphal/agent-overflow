// Long-lived WebSocket client.
//
// Owns:
//   - The (single) WebSocket connection and its lifecycle.
//   - In-flight RPC tracking, by request id.
//   - Per-channel last-seen seq for replay-on-reconnect.
//   - Subscriber fanout (one tokio::sync::broadcast per channel).
//   - Transport status (watch channel the UI subscribes to).
//
// Architecture:
//   - Outer `Transport` struct is cheap-clonable (Arc inside) and the
//     only handle the rest of the crate uses.
//   - One supervisor task per Transport runs the connect / read / write
//     loop. It holds the actual socket; read frames feed an internal
//     dispatch routine, write frames come from a tokio::mpsc channel
//     fed by `Transport::call*` and `Transport::replay`.
//   - `call_*` returns a future that resolves when a matching `rpc`
//     reply lands. We track futures by id with a oneshot sender stored
//     in a Mutex<HashMap<String, ...>>.

use std::collections::HashMap;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use futures_util::{SinkExt, StreamExt};
use parking_lot::Mutex;
use serde_json::Value;
use thiserror::Error;
use tokio::sync::{Notify, broadcast, mpsc, oneshot, watch};
use tokio_tungstenite::tungstenite::Message;
use url::Url;

use super::wire::{ClientFrame, FrameError, ServerFrame};

/// Conservative cap on outstanding RPCs. Mirrors the wsClient.ts cap
/// (10_000) — at that point something pathological is happening.
const MAX_PENDING_RPCS: usize = 10_000;
const RPC_TIMEOUT: Duration = Duration::from_secs(30);
const RECONNECT_INITIAL: Duration = Duration::from_millis(250);
const RECONNECT_MAX: Duration = Duration::from_secs(30);
const PER_CHANNEL_BUFFER: usize = 256;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TransportStatus {
    Disconnected,
    Connecting,
    Connected,
    Reconnecting,
}

#[derive(Debug, Error)]
pub enum TransportError {
    #[error("rpc error: {code}: {message}")]
    Rpc { code: String, message: String },
    #[error("transport disconnected")]
    Disconnected,
    #[error("rpc timed out after {0:?}")]
    Timeout(Duration),
    #[error("decode error: {0}")]
    Decode(#[from] serde_json::Error),
    #[error("client overloaded: too many concurrent RPCs (>= {0})")]
    Overloaded(usize),
    #[error("transport closed by caller")]
    Closed,
    #[error(transparent)]
    Other(#[from] anyhow::Error),
}

impl From<FrameError> for TransportError {
    fn from(e: FrameError) -> Self {
        Self::Rpc {
            code: e.code,
            message: e.message,
        }
    }
}

type Pending = HashMap<String, oneshot::Sender<Result<Value, TransportError>>>;

struct Inner {
    pending: Mutex<Pending>,
    next_id: AtomicU64,
    write_tx: mpsc::UnboundedSender<Message>,
    status_rx: watch::Receiver<TransportStatus>,
    /// Channel name → broadcast sender. Subscribers get their own
    /// `broadcast::Receiver` and receive only events on that channel.
    /// Senders are created lazily on first subscribe.
    channels: Mutex<HashMap<String, broadcast::Sender<Value>>>,
    /// Per-channel last-seq we've delivered to local subscribers. Used
    /// for replay-on-reconnect; see Transport::send_replay.
    last_seq: Mutex<HashMap<String, u64>>,
    /// Notify supervisor loop to shut down.
    closed: Arc<Notify>,
}

#[derive(Clone)]
pub struct Transport {
    inner: Arc<Inner>,
}

impl Transport {
    /// Connect to `ws_url` (already-bootstrapped) using `token` as the
    /// `?token=...` query parameter. Spawns a supervisor task on the
    /// current tokio runtime.
    pub fn connect(ws_url: &str, token: &str) -> Result<Self, TransportError> {
        let url = append_token(ws_url, token).map_err(|e| {
            TransportError::Other(anyhow::anyhow!("bad ws url: {e}"))
        })?;

        let (write_tx, write_rx) = mpsc::unbounded_channel::<Message>();
        let (status_tx, status_rx) = watch::channel(TransportStatus::Connecting);
        let closed = Arc::new(Notify::new());

        let inner = Arc::new(Inner {
            pending: Mutex::new(HashMap::new()),
            next_id: AtomicU64::new(1),
            write_tx,
            status_rx,
            channels: Mutex::new(HashMap::new()),
            last_seq: Mutex::new(HashMap::new()),
            closed: closed.clone(),
        });

        tokio::spawn(supervisor(
            url,
            write_rx,
            status_tx,
            inner.clone(),
            closed,
        ));

        Ok(Self { inner })
    }

    /// Subscribe to the transport status. Receivers see the current
    /// snapshot synchronously and subsequent updates via .changed().
    pub fn status(&self) -> watch::Receiver<TransportStatus> {
        self.inner.status_rx.clone()
    }

    /// Subscribe to an event channel. The first call for a channel
    /// creates the broadcast lane lazily; subsequent calls fan out from
    /// the same lane. Receivers that lag get tokio's standard
    /// `RecvError::Lagged` and can choose to refresh from RPC.
    pub fn subscribe(&self, channel: &str) -> broadcast::Receiver<Value> {
        let mut channels = self.inner.channels.lock();
        let sender = channels
            .entry(channel.to_string())
            .or_insert_with(|| broadcast::channel(PER_CHANNEL_BUFFER).0);
        sender.subscribe()
    }

    /// Invoke an RPC by methodId. Resolves with the deserialized
    /// `result` field of the response; returns TransportError otherwise.
    pub async fn call_id(
        &self,
        method_id: u32,
        params: Vec<Value>,
    ) -> Result<Value, TransportError> {
        let (id, rx) = self.register()?;
        let frame = ClientFrame::Rpc {
            id: id.clone(),
            method_id: Some(method_id),
            method: None,
            params,
        };
        self.dispatch(frame, id, rx).await
    }

    /// Invoke an RPC by name. Hand-written code paths (none today) and
    /// debugging both want this; the Svelte runtime exposes
    /// Call.ByName for the same reason.
    pub async fn call_name(
        &self,
        method: &str,
        params: Vec<Value>,
    ) -> Result<Value, TransportError> {
        let (id, rx) = self.register()?;
        let frame = ClientFrame::Rpc {
            id: id.clone(),
            method_id: None,
            method: Some(method.to_string()),
            params,
        };
        self.dispatch(frame, id, rx).await
    }

    fn register(
        &self,
    ) -> Result<(String, oneshot::Receiver<Result<Value, TransportError>>), TransportError> {
        let mut pending = self.inner.pending.lock();
        if pending.len() >= MAX_PENDING_RPCS {
            return Err(TransportError::Overloaded(MAX_PENDING_RPCS));
        }
        let id = self.next_id();
        let (tx, rx) = oneshot::channel();
        pending.insert(id.clone(), tx);
        Ok((id, rx))
    }

    fn next_id(&self) -> String {
        let n = self.inner.next_id.fetch_add(1, Ordering::Relaxed);
        format!("{n}")
    }

    async fn dispatch(
        &self,
        frame: ClientFrame,
        id: String,
        rx: oneshot::Receiver<Result<Value, TransportError>>,
    ) -> Result<Value, TransportError> {
        let payload = serde_json::to_string(&frame)?;
        if self
            .inner
            .write_tx
            .send(Message::Text(payload.into()))
            .is_err()
        {
            // Writer dropped — supervisor is closing or dead. Surface
            // disconnected and clear the pending entry.
            self.inner.pending.lock().remove(&id);
            return Err(TransportError::Disconnected);
        }

        match tokio::time::timeout(RPC_TIMEOUT, rx).await {
            Ok(Ok(result)) => result,
            Ok(Err(_)) => {
                // Sender dropped without sending — happens on disconnect.
                self.inner.pending.lock().remove(&id);
                Err(TransportError::Disconnected)
            }
            Err(_) => {
                self.inner.pending.lock().remove(&id);
                Err(TransportError::Timeout(RPC_TIMEOUT))
            }
        }
    }

    /// Tear the transport down. Future RPCs reject; subscribers see
    /// their broadcast lanes close as senders drop.
    pub fn close(&self) {
        self.inner.closed.notify_waiters();
    }
}

fn append_token(ws_url: &str, token: &str) -> Result<Url, url::ParseError> {
    let mut url = Url::parse(ws_url)?;
    url.query_pairs_mut().append_pair("token", token);
    Ok(url)
}

async fn supervisor(
    url: Url,
    mut write_rx: mpsc::UnboundedReceiver<Message>,
    status_tx: watch::Sender<TransportStatus>,
    inner: Arc<Inner>,
    closed: Arc<Notify>,
) {
    let mut attempt: u32 = 0;

    loop {
        // Yield to supervisor close before each connect cycle so the
        // first iteration after `close()` exits cleanly without a
        // spurious dial.
        if status_tx.receiver_count() == 0 {
            // No status subscribers AND closed — bail. We don't want to
            // hard-stop on no-subscribers because the UI may not have
            // subscribed yet at the very beginning.
        }

        if attempt > 0 {
            let delay = backoff(attempt);
            tracing::debug!(attempt, ?delay, "reconnect backoff");
            let _ = status_tx.send(TransportStatus::Reconnecting);
            tokio::select! {
                _ = tokio::time::sleep(delay) => {},
                _ = closed.notified() => {
                    tracing::debug!("supervisor: closed during backoff");
                    return;
                }
            }
        }

        let _ = status_tx.send(TransportStatus::Connecting);
        let socket = match tokio_tungstenite::connect_async(url.as_str()).await {
            Ok((ws, _resp)) => ws,
            Err(e) => {
                tracing::warn!(error = %e, "ws connect failed");
                attempt = attempt.saturating_add(1);
                continue;
            }
        };

        let _ = status_tx.send(TransportStatus::Connected);

        // Send a replay frame on every (re)connect. The Go side ignores
        // an empty lastSeqByChannel map — it only replays for channels
        // we've seen at least once before. So this is safe even on
        // first connect.
        let replay = ClientFrame::Replay {
            last_seq_by_channel: inner.last_seq.lock().clone(),
        };
        if let Ok(text) = serde_json::to_string(&replay) {
            let _ = inner.write_tx.send(Message::Text(text.into()));
        }

        let (mut sink, mut stream) = socket.split();

        // Reader pumps incoming frames into the shared dispatcher.
        let inner_for_read = inner.clone();
        let read_done = Arc::new(Notify::new());
        let read_done_signal = read_done.clone();
        let reader_handle = tokio::spawn(async move {
            while let Some(msg) = stream.next().await {
                match msg {
                    Ok(Message::Text(text)) => {
                        match serde_json::from_str::<ServerFrame>(&text) {
                            Ok(frame) => dispatch_inbound(&inner_for_read, frame),
                            Err(e) => tracing::warn!(error=%e, "decode server frame"),
                        }
                    }
                    Ok(Message::Binary(_)) => {
                        tracing::warn!("unexpected binary frame; ignoring");
                    }
                    Ok(Message::Close(frame)) => {
                        tracing::info!(?frame, "server closed connection");
                        break;
                    }
                    Ok(Message::Ping(_) | Message::Pong(_) | Message::Frame(_)) => {}
                    Err(e) => {
                        tracing::warn!(error=%e, "ws read error");
                        break;
                    }
                }
            }
            read_done_signal.notify_waiters();
        });

        // Writer drains outbound frames until the reader signals close
        // OR the close notify fires.
        loop {
            tokio::select! {
                msg = write_rx.recv() => {
                    let Some(msg) = msg else { break; };
                    if let Err(e) = sink.send(msg).await {
                        tracing::warn!(error=%e, "ws write error");
                        break;
                    }
                }
                _ = read_done.notified() => break,
                _ = closed.notified() => {
                    let _ = sink.send(Message::Close(None)).await;
                    reader_handle.abort();
                    fail_pending(&inner);
                    let _ = status_tx.send(TransportStatus::Disconnected);
                    return;
                }
            }
        }

        // Reader closed or writer errored; tear down and reconnect.
        reader_handle.abort();
        fail_pending(&inner);
        attempt = 1;
    }
}

fn dispatch_inbound(inner: &Arc<Inner>, frame: ServerFrame) {
    match frame {
        ServerFrame::Rpc { id, result, error } => {
            let waiter = inner.pending.lock().remove(&id);
            if let Some(tx) = waiter {
                let outcome = match (result, error) {
                    (_, Some(err)) => Err(TransportError::from(err)),
                    (Some(value), None) => Ok(value),
                    (None, None) => Ok(Value::Null),
                };
                let _ = tx.send(outcome);
            } else {
                tracing::debug!(id, "rpc reply for unknown id; dropping");
            }
        }
        ServerFrame::Event {
            channel,
            seq,
            data,
            gap,
        } => {
            inner.last_seq.lock().insert(channel.clone(), seq);
            if gap {
                tracing::warn!(channel, seq, "transport gap on channel");
                if let Some(tx) = inner
                    .channels
                    .lock()
                    .get(crate::transport::client::TRANSPORT_GAP_CHANNEL)
                    .cloned()
                {
                    let _ = tx.send(serde_json::json!({
                        "channel": channel,
                        "seq": seq,
                    }));
                }
            }
            if let Some(tx) = inner.channels.lock().get(&channel).cloned() {
                let _ = tx.send(data);
            }
        }
    }
}

fn fail_pending(inner: &Arc<Inner>) {
    let waiters: Vec<_> = inner
        .pending
        .lock()
        .drain()
        .map(|(_, tx)| tx)
        .collect();
    for tx in waiters {
        let _ = tx.send(Err(TransportError::Disconnected));
    }
}

fn backoff(attempt: u32) -> Duration {
    // Capped exponential. attempt is 1-based when supervisor enters
    // backoff for the first time.
    let exp: u32 = attempt.saturating_sub(1).min(8);
    let scaled = RECONNECT_INITIAL.checked_mul(1u32 << exp).unwrap_or(RECONNECT_MAX);
    scaled.min(RECONNECT_MAX)
}

/// Synthetic channel mirroring the Svelte client's `transport:gap`
/// signal. Subscribers see one event per gap detected on any underlying
/// channel; the payload is `{channel, seq}`.
pub const TRANSPORT_GAP_CHANNEL: &str = "transport:gap";

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn append_token_preserves_path() {
        let url = append_token("ws://127.0.0.1:1234/ws", "secret").unwrap();
        assert_eq!(url.path(), "/ws");
        assert_eq!(url.query(), Some("token=secret"));
    }

    #[test]
    fn backoff_caps_at_max() {
        for n in 1..50 {
            assert!(backoff(n) <= RECONNECT_MAX);
        }
    }
}
