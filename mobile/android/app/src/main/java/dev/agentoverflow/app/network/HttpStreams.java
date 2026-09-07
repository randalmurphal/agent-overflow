package dev.agentoverflow.app.network;

import java.io.IOException;
import java.util.HashMap;
import java.util.Map;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.atomic.AtomicBoolean;
import okhttp3.Call;
import okhttp3.Callback;
import okhttp3.Request;
import okhttp3.Response;

/** HTTP bodies have independent, bounded lifetimes. The bridge pulls one chunk
 * at a time; a dead renderer is reclaimed by the plugin's idle sweep. */
public final class HttpStreams implements AutoCloseable {
    public static final int MAX_TRANSFERS = 16;
    private final PinnedClients clients;
    private final Map<String, Transfer> transfers = new HashMap<>();

    public static final class Transfer {
        public final CompletableFuture<Response> headers = new CompletableFuture<>();
        private final UploadPipe upload;
        private final AtomicBoolean reading = new AtomicBoolean();
        private final AtomicBoolean writing = new AtomicBoolean();
        private volatile long touched = System.nanoTime();
        private volatile Response response;
        private Call call;
        private long received;
        private boolean closed;

        Transfer(UploadPipe upload) { this.upload = upload; }

        synchronized void received(Response response) {
            if (closed) { response.close(); return; }
            this.response = response;
            touched = System.nanoTime();
            headers.complete(response);
        }

        public void write(byte[] bytes, boolean end) throws IOException {
            if (upload == null) throw new IOException("This request has no upload body");
            if (!writing.compareAndSet(false, true)) throw new IOException("Upload write already pending");
            touched = System.nanoTime();
            try { upload.write(bytes, end); }
            finally { touched = System.nanoTime(); writing.set(false); }
        }

        public byte[] read() throws IOException {
            Response reply = response;
            if (reply == null) throw new IOException("Response headers have not arrived");
            if (!reading.compareAndSet(false, true)) throw new IOException("Download read already pending");
            touched = System.nanoTime();
            try {
                byte[] buffer = new byte[UploadPipe.CHUNK_BYTES];
                int count = reply.body().byteStream().read(buffer);
                if (count < 0) return new byte[0];
                received += count;
                if (received > 128L * 1024 * 1024) throw new IOException("Download exceeds the size limit");
                return count == buffer.length ? buffer : java.util.Arrays.copyOf(buffer, count);
            } finally { touched = System.nanoTime(); reading.set(false); }
        }

        void close() { close(new IOException("Transfer closed")); }

        synchronized void close(IOException failure) {
            if (closed) return;
            closed = true;
            if (upload != null) upload.cancel(failure);
            call.cancel();
            if (response != null) response.close();
            headers.completeExceptionally(failure);
        }
    }

    public HttpStreams(PinnedClients clients) { this.clients = clients; }

    public synchronized Transfer start(String id, String url, String pin, String method,
                                       Map<String, String> headers, long length) throws Exception {
        if (!id.matches("[a-zA-Z0-9-]{1,80}") || transfers.containsKey(id)) throw new IOException("Invalid transfer id");
        if (transfers.size() >= MAX_TRANSFERS) throw new IOException("Too many file transfers. Wait for one to finish.");
        if (!method.matches("GET|HEAD|POST|PUT|DELETE|PATCH|OPTIONS")) throw new IOException("Invalid HTTP method");
        long bodyLength = length < 0 && method.matches("POST|PUT|PATCH") ? 0 : length;
        UploadPipe body = bodyLength < 0 ? null : new UploadPipe(bodyLength, headers.getOrDefault("content-type", "application/octet-stream"));
        Request.Builder request = new Request.Builder().url(PinnedClients.endpoint(url)).method(method, body);
        for (var header : headers.entrySet()) {
            String name = header.getKey();
            if (name.equalsIgnoreCase("cookie") || name.equalsIgnoreCase("host")
                    || name.equalsIgnoreCase("connection") || name.equalsIgnoreCase("content-length")) continue;
            request.header(name, header.getValue());
        }
        request.header("Origin", "https://shell.agent-overflow.invalid");
        Transfer transfer = new Transfer(body);
        transfer.call = clients.forPin(pin).newCall(request.build());
        transfers.put(id, transfer);
        transfer.call.enqueue(new Callback() {
            public void onFailure(Call call, IOException failure) {
                // Keep the completed failure until the renderer reads headers
                // (or the idle sweep reclaims it). Removing the handle here
                // races httpHeaders and hides a certificate error as "gone".
                // The body writer can reach the bridge before its header
                // reader; both must retain the original network failure.
                transfer.close(failure);
            }
            public void onResponse(Call call, Response response) { transfer.received(response); }
        });
        return transfer;
    }

    public synchronized Transfer get(String id) throws IOException {
        Transfer transfer = transfers.get(id);
        if (transfer == null) throw new IOException("Transfer is no longer available");
        return transfer;
    }

    public synchronized void close(String id) {
        Transfer transfer = transfers.remove(id);
        if (transfer != null) transfer.close();
    }

    public synchronized void sweep() {
        long stale = System.nanoTime() - java.util.concurrent.TimeUnit.MINUTES.toNanos(2);
        transfers.entrySet().removeIf(entry -> {
            if (entry.getValue().touched > stale) return false;
            entry.getValue().close();
            return true;
        });
    }

    @Override public synchronized void close() {
        for (Transfer transfer : transfers.values()) transfer.close();
        transfers.clear();
    }
}
