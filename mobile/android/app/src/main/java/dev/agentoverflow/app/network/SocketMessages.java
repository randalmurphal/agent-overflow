package dev.agentoverflow.app.network;

import java.util.ArrayDeque;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.TimeUnit;
import java.util.function.Consumer;

/** One acknowledged bridge delivery at a time, with a bounded queue behind it.
 * Budgets include the delivery JS owns until its acknowledgement. */
final class SocketMessages {
    static final int MAX_MESSAGES = 64;
    static final int MAX_CHARS = 256 * 1024;
    private final boolean batchMessages;
    private final int maxMessages;
    private final int maxChars;
    private final long timeoutNanos;
    private final Consumer<Batch> deliver;
    private final ArrayDeque<String> queued = new ArrayDeque<>();
    private int retainedCount, retainedChars, inFlightCount, inFlightChars, sequence;
    private boolean closed;

    static final class Batch {
        final int sequence;
        final List<String> messages;
        Batch(int sequence, List<String> messages) {
            this.sequence = sequence;
            this.messages = messages;
        }
    }

    SocketMessages(boolean batchMessages, Consumer<Batch> deliver) {
        this(batchMessages, MAX_MESSAGES, MAX_CHARS, TimeUnit.SECONDS.toNanos(60), deliver);
    }

    SocketMessages(boolean batchMessages, int maxMessages, int maxChars, long timeoutNanos, Consumer<Batch> deliver) {
        this.batchMessages = batchMessages;
        this.maxMessages = batchMessages ? maxMessages : 1;
        this.maxChars = maxChars;
        this.timeoutNanos = timeoutNanos;
        this.deliver = deliver;
    }

    boolean add(String text) throws InterruptedException {
        Batch batch;
        synchronized (this) {
            long deadline = System.nanoTime() + timeoutNanos;
            // One oversized frame is allowed only when nothing else is retained.
            while (!closed && retainedCount > 0 &&
                    (retainedCount >= maxMessages || text.length() > maxChars - retainedChars)) {
                long remaining = deadline - System.nanoTime();
                if (remaining <= 0) return false;
                TimeUnit.NANOSECONDS.timedWait(this, remaining);
            }
            if (closed) return false;
            queued.add(text);
            retainedCount++;
            retainedChars += text.length();
            batch = takeBatch();
        }
        if (batch != null) deliver.accept(batch);
        return true;
    }

    void acknowledge(int acknowledgedSequence) {
        Batch batch;
        synchronized (this) {
            if (closed || inFlightCount == 0 || (batchMessages && acknowledgedSequence != sequence)) return;
            retainedCount -= inFlightCount;
            retainedChars -= inFlightChars;
            inFlightCount = 0;
            inFlightChars = 0;
            batch = takeBatch();
            notifyAll();
        }
        if (batch != null) deliver.accept(batch);
    }

    // The first message is immediate. While it is in JS, the reader fills the
    // queue; its ACK drains that queue with no coalescing timer or extra thread.
    private Batch takeBatch() {
        if (inFlightCount != 0 || queued.isEmpty()) return null;
        List<String> messages = new ArrayList<>(queued.size());
        while (!queued.isEmpty()) {
            String text = queued.remove();
            messages.add(text);
            inFlightChars += text.length();
        }
        inFlightCount = messages.size();
        sequence = sequence == Integer.MAX_VALUE ? 1 : sequence + 1;
        return new Batch(sequence, messages);
    }

    /** A normal peer close follows every accepted message, including the queue. */
    synchronized boolean awaitDrained() throws InterruptedException {
        long deadline = System.nanoTime() + timeoutNanos;
        while (!closed && retainedCount != 0) {
            long remaining = deadline - System.nanoTime();
            if (remaining <= 0) return false;
            TimeUnit.NANOSECONDS.timedWait(this, remaining);
        }
        return !closed;
    }

    synchronized void close() {
        closed = true;
        queued.clear();
        retainedCount = retainedChars = inFlightCount = inFlightChars = 0;
        notifyAll();
    }
}
