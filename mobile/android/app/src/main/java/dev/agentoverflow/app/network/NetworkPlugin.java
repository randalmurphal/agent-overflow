package dev.agentoverflow.app.network;

import com.getcapacitor.JSObject;
import com.getcapacitor.Plugin;
import com.getcapacitor.PluginCall;
import com.getcapacitor.PluginMethod;
import com.getcapacitor.annotation.CapacitorPlugin;
import java.util.Base64;
import java.util.HashMap;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.Executors;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.ThreadPoolExecutor;
import java.util.concurrent.SynchronousQueue;
import java.util.concurrent.RejectedExecutionException;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.TimeUnit;
import okhttp3.Request;
import okhttp3.Response;
import okhttp3.WebSocket;
import okhttp3.WebSocketListener;
import okio.ByteString;

/** Verified TLS, pinned or platform WebPKI. This bridge carries the same HTTP/WS protocol;
 * no RPC dispatch, auth policy or reconnect policy belongs in Java. */
@CapacitorPlugin(name = "Network")
public class NetworkPlugin extends Plugin {
    private final PinnedClients clients = new PinnedClients();
    private final HttpStreams http = new HttpStreams(clients);
    private static final class SocketSlot {
        volatile WebSocket socket;
        final java.util.concurrent.Semaphore delivery = new java.util.concurrent.Semaphore(1);
        volatile boolean closed;
        void close() {
            closed = true;
            delivery.release();
            if (socket != null) socket.cancel();
        }
    }
    private final Map<String, SocketSlot> sockets = new ConcurrentHashMap<>();
    // Blocking body reads/writes must never occupy Capacitor's single plugin
    // thread, which also delivers unlock, Back and socket sends.
    private final ExecutorService io = new ThreadPoolExecutor(0, HttpStreams.MAX_TRANSFERS * 2,
            30, TimeUnit.SECONDS, new SynchronousQueue<>());
    private final ScheduledExecutorService sweep = Executors.newSingleThreadScheduledExecutor();

    @Override public void load() { sweep.scheduleAtFixedRate(http::sweep, 30, 30, TimeUnit.SECONDS); }

    @PluginMethod public void getCapabilities(PluginCall call) {
        JSObject result = new JSObject();
        result.put("computerRoutes", true);
        call.resolve(result);
    }

    @PluginMethod public void httpStart(PluginCall call) {
        try {
            Map<String, String> headers = new HashMap<>();
            JSObject input = call.getObject("headers", new JSObject());
            for (var keys = input.keys(); keys.hasNext();) {
                String key = keys.next();
                headers.put(key.toLowerCase(java.util.Locale.ROOT), input.getString(key));
            }
            http.start(call.getString("id", ""), call.getString("url", ""), call.getString("pin", ""),
                    call.getString("method", "GET"), headers, call.getInt("length", -1));
            call.resolve();
        } catch (Exception e) { call.reject(failureMessage(e)); }
    }

    @PluginMethod public void httpHeaders(PluginCall call) {
        try {
            http.get(call.getString("id", "")).headers.whenComplete((response, failure) -> {
                if (failure != null) { call.reject(failureMessage(failure)); return; }
                JSObject headers = new JSObject();
                for (String name : response.headers().names()) {
                    if (!name.equalsIgnoreCase("set-cookie")) headers.put(name, response.header(name));
                }
                JSObject result = new JSObject();
                result.put("status", response.code());
                result.put("headers", headers);
                call.resolve(result);
            });
        } catch (Exception e) { call.reject(failureMessage(e)); }
    }

    @PluginMethod public void httpWrite(PluginCall call) {
        String id = call.getString("id", "");
        String data = call.getString("data", "");
        if (data.length() > (UploadPipe.CHUNK_BYTES + 2) / 3 * 4) {
            call.reject("Upload chunk is too large"); return;
        }
        runIO(call, () -> {
            try {
                http.get(id).write(Base64.getDecoder().decode(data), call.getBoolean("end", false));
                call.resolve();
            } catch (Exception e) { http.close(id); call.reject(failureMessage(e)); }
        });
    }

    @PluginMethod public void httpRead(PluginCall call) {
        String id = call.getString("id", "");
        runIO(call, () -> {
            try {
                byte[] bytes = http.get(id).read();
                JSObject result = new JSObject();
                result.put("data", Base64.getEncoder().encodeToString(bytes));
                if (bytes.length == 0) http.close(id);
                call.resolve(result);
            } catch (Exception e) { http.close(id); call.reject(failureMessage(e)); }
        });
    }

    @PluginMethod public void httpClose(PluginCall call) {
        http.close(call.getString("id", ""));
        call.resolve();
    }

    @PluginMethod public void socketOpen(PluginCall call) {
        String id = call.getString("id", "");
        if (!id.matches("[a-zA-Z0-9-]{1,80}") || sockets.containsKey(id) || sockets.size() >= 32) {
            call.reject("Too many connections or invalid connection id"); return;
        }
        SocketSlot slot = new SocketSlot();
        sockets.put(id, slot);
        try {
            Request request = new Request.Builder().url(PinnedClients.endpoint(call.getString("url", "")))
                    .header("Origin", "https://shell.agent-overflow.invalid").build();
            WebSocket socket = clients.forPin(call.getString("pin", "")).newWebSocket(request, new WebSocketListener() {
                @Override public void onOpen(WebSocket ws, Response response) { event(id, "open", "", 0); }
                @Override public void onMessage(WebSocket ws, String text) {
                    if (text.length() > 75 * 1024 * 1024) { ws.close(1009, "Frame too large"); return; }
                    try {
                        if (!slot.delivery.tryAcquire(60, TimeUnit.SECONDS) || slot.closed) {
                            ws.cancel(); return;
                        }
                        event(id, "message", text, 0);
                    } catch (InterruptedException interrupted) {
                        Thread.currentThread().interrupt(); ws.cancel();
                    }
                }
                @Override public void onMessage(WebSocket ws, ByteString bytes) { ws.close(1003, "Text frames required"); }
                @Override public void onClosing(WebSocket ws, int code, String reason) { ws.close(code, reason); }
                @Override public void onClosed(WebSocket ws, int code, String reason) {
                    sockets.remove(id, slot); slot.close(); event(id, "close", reason, code);
                }
                @Override public void onFailure(WebSocket ws, Throwable failure, Response response) {
                    sockets.remove(id, slot); slot.close(); event(id, "error", failureMessage(failure), 0);
                    event(id, "close", "", 1006);
                }
            });
            slot.socket = socket;
            if (slot.closed) socket.cancel();
            call.resolve();
        } catch (Exception e) { sockets.remove(id, slot); slot.close(); call.reject(failureMessage(e)); }
    }

    @PluginMethod public void socketAck(PluginCall call) {
        SocketSlot slot = sockets.get(call.getString("id", ""));
        if (slot != null && slot.delivery.availablePermits() == 0) slot.delivery.release();
        call.resolve();
    }

    @PluginMethod public void socketSend(PluginCall call) {
        SocketSlot slot = sockets.get(call.getString("id", ""));
        if (slot == null || slot.socket == null || !slot.socket.send(call.getString("data", ""))) {
            call.reject("Connection closed or its send buffer is full"); return;
        }
        call.resolve();
    }

    @PluginMethod public void socketClose(PluginCall call) {
        SocketSlot slot = sockets.remove(call.getString("id", ""));
        if (slot != null) slot.close();
        call.resolve();
    }

    private void runIO(PluginCall call, Runnable task) {
        try { io.execute(task); }
        catch (RejectedExecutionException full) { call.reject("Too many pending transfers. Try again shortly."); }
    }

    private void event(String id, String type, String data, int code) {
        JSObject event = new JSObject();
        event.put("id", id); event.put("type", type); event.put("data", data); event.put("code", code);
        notifyListeners("socket", event);
    }

    static String failureMessage(Throwable failure) {
        java.util.ArrayDeque<Throwable> causes = new java.util.ArrayDeque<>();
        causes.add(failure);
        for (int checked = 0; !causes.isEmpty() && checked < 32; checked++) {
            Throwable cause = causes.removeFirst();
            if (cause instanceof java.security.cert.CertificateException) {
                return "The computer's certificate could not be verified. Pair it again to verify its identity.";
            }
            if (cause.getCause() != null) causes.add(cause.getCause());
            for (Throwable suppressed : cause.getSuppressed()) causes.add(suppressed);
        }
        // Network exceptions can include a URL carrying a single-use ticket.
        // Report a useful class without logging that credential through JS.
        if (failure instanceof java.net.SocketTimeoutException) return "The computer did not respond in time";
        if (failure instanceof java.net.ConnectException) return "The computer could not be reached";
        if (failure instanceof java.net.UnknownHostException) return "The computer's address could not be found";
        return "The connection failed (" + failure.getClass().getSimpleName() + ")";
    }

    @Override protected void handleOnDestroy() {
        sweep.shutdownNow();
        http.close();
        for (SocketSlot slot : sockets.values()) slot.close();
        sockets.clear();
        clients.close();
        io.shutdownNow();
        super.handleOnDestroy();
    }
}
