package dev.agentoverflow.app.network;

import java.io.IOException;
import okhttp3.MediaType;
import okhttp3.RequestBody;
import okio.BufferedSink;

/** One 64 KiB chunk of backpressure between the bridge and the HTTP body.
 * A whole file is never represented as a Java or JavaScript string. */
final class UploadPipe extends RequestBody {
    static final int CHUNK_BYTES = 64 * 1024;
    static final long MAX_BYTES = 50L * 1024 * 1024;
    private final long length;
    private final MediaType type;
    private byte[] chunk;
    private long written;
    private boolean ended;
    private IOException failure;

    UploadPipe(long length, String type) {
        if (length < 0 || length > MAX_BYTES) throw new IllegalArgumentException("Upload exceeds the file size limit");
        this.length = length;
        this.ended = length == 0;
        this.type = MediaType.parse(type);
    }

    @Override public long contentLength() { return length; }
    @Override public MediaType contentType() { return type; }
    @Override public boolean isOneShot() { return true; }

    synchronized void write(byte[] bytes, boolean end) throws IOException {
        if (bytes.length > CHUNK_BYTES) throw new IOException("Upload chunk is too large");
        while (chunk != null && failure == null) awaitProgress();
        if (failure != null) throw failure;
        if (ended && length == 0 && bytes.length == 0 && end) return;
        if (ended) throw new IOException("Upload is already closed");
        if (written + bytes.length > length || (end && written + bytes.length != length)) {
            throw new IOException("Upload body does not match its declared length");
        }
        written += bytes.length;
        if (bytes.length > 0) chunk = bytes;
        ended = end;
        notifyAll();
    }

    private synchronized byte[] take() throws IOException {
        while (chunk == null && !ended && failure == null) awaitProgress();
        if (failure != null) throw failure;
        byte[] next = chunk;
        chunk = null;
        notifyAll();
        return next;
    }

    private void awaitProgress() throws IOException {
        try { wait(60_000); }
        catch (InterruptedException e) { Thread.currentThread().interrupt(); throw new IOException("Upload interrupted", e); }
    }

    synchronized void cancel(IOException cause) {
        if (failure == null) failure = cause;
        chunk = null;
        notifyAll();
    }

    @Override public void writeTo(BufferedSink sink) throws IOException {
        for (byte[] next; (next = take()) != null;) sink.write(next);
    }
}
