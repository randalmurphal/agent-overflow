package dev.agentoverflow.app.network;

import org.junit.Test;
import static org.junit.Assert.*;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;

public class SocketMessagesTest {
    @Test public void firstMessageIsImmediateThenAcknowledgementsDrainOrderedBatches() throws Exception {
        List<SocketMessages.Batch> deliveries = new ArrayList<>();
        SocketMessages messages = new SocketMessages(true, deliveries::add);
        assertTrue(messages.add("first"));
        assertEquals(List.of("first"), deliveries.get(0).messages);
        assertTrue(messages.add("second"));
        assertTrue(messages.add("third"));
        assertEquals(1, deliveries.size());
        messages.acknowledge(deliveries.get(0).sequence);
        assertEquals(List.of("second", "third"), deliveries.get(1).messages);
        assertTrue(messages.add("fourth"));
        messages.acknowledge(deliveries.get(0).sequence);
        messages.acknowledge(-1);
        assertEquals(2, deliveries.size());
        messages.acknowledge(deliveries.get(1).sequence);
        assertEquals(List.of("fourth"), deliveries.get(2).messages);
        messages.acknowledge(deliveries.get(2).sequence);
        messages.acknowledge(deliveries.get(2).sequence);
        assertTrue(messages.awaitDrained());
    }

    @Test public void countBudgetIncludesTheInFlightBatchAndCloseUnblocksProducer() throws Exception {
        List<SocketMessages.Batch> deliveries = new CopyOnWriteArrayList<>();
        SocketMessages messages = new SocketMessages(true, 3, 100, TimeUnit.SECONDS.toNanos(2), deliveries::add);
        try (var executor = Executors.newSingleThreadExecutor()) {
            assertTrue(messages.add("one"));
            assertTrue(messages.add("two"));
            assertTrue(messages.add("three"));
            CountDownLatch entered = new CountDownLatch(1);
            var fourth = executor.submit(() -> { entered.countDown(); return messages.add("four"); });
            assertTrue(entered.await(1, TimeUnit.SECONDS));
            assertThrows(TimeoutException.class, () -> fourth.get(50, TimeUnit.MILLISECONDS));
            messages.acknowledge(deliveries.get(0).sequence);
            assertTrue(fourth.get(1, TimeUnit.SECONDS));
            assertEquals(List.of("two", "three"), deliveries.get(1).messages);
            var fifth = executor.submit(() -> messages.add("five"));
            assertThrows(TimeoutException.class, () -> fifth.get(50, TimeUnit.MILLISECONDS));
            messages.close();
            assertFalse(fifth.get(1, TimeUnit.SECONDS));
            messages.acknowledge(deliveries.get(1).sequence);
            assertFalse(messages.add("after close"));
            assertEquals(2, deliveries.size());
        }
    }

    @Test public void characterBudgetAllowsOneOversizedFrameAlone() throws Exception {
        List<SocketMessages.Batch> deliveries = new CopyOnWriteArrayList<>();
        SocketMessages messages = new SocketMessages(true, 64, 4, TimeUnit.SECONDS.toNanos(2), deliveries::add);
        try (var executor = Executors.newSingleThreadExecutor()) {
            assertTrue(messages.add("four"));
            var oversized = executor.submit(() -> messages.add("oversized"));
            assertThrows(TimeoutException.class, () -> oversized.get(50, TimeUnit.MILLISECONDS));
            messages.acknowledge(deliveries.get(0).sequence);
            assertTrue(oversized.get(1, TimeUnit.SECONDS));
            assertEquals(List.of("oversized"), deliveries.get(1).messages);
            var following = executor.submit(() -> messages.add("x"));
            assertThrows(TimeoutException.class, () -> following.get(50, TimeUnit.MILLISECONDS));
            messages.acknowledge(deliveries.get(1).sequence);
            assertTrue(following.get(1, TimeUnit.SECONDS));
            assertEquals(List.of("x"), deliveries.get(2).messages);
            messages.close();
        }
    }

    @Test public void legacyClientsStillReceiveOneMessagePerUnsequencedAcknowledgement() throws Exception {
        List<SocketMessages.Batch> deliveries = new CopyOnWriteArrayList<>();
        SocketMessages messages = new SocketMessages(false, deliveries::add);
        try (var executor = Executors.newSingleThreadExecutor()) {
            assertTrue(messages.add("old SPA first"));
            var second = executor.submit(() -> messages.add("old SPA second"));
            assertThrows(TimeoutException.class, () -> second.get(50, TimeUnit.MILLISECONDS));
            messages.acknowledge(-1);
            assertTrue(second.get(1, TimeUnit.SECONDS));
            assertEquals(List.of("old SPA second"), deliveries.get(1).messages);
            messages.acknowledge(-1);
            assertTrue(messages.awaitDrained());
        }
    }

    @Test public void peerCloseWaitsForTheLastQueuedDeliveryAndCancellationWakesIt() throws Exception {
        List<SocketMessages.Batch> deliveries = new CopyOnWriteArrayList<>();
        SocketMessages messages = new SocketMessages(true, deliveries::add);
        try (var executor = Executors.newSingleThreadExecutor()) {
            messages.add("one");
            messages.add("two");
            var drained = executor.submit(messages::awaitDrained);
            messages.acknowledge(deliveries.get(0).sequence);
            assertThrows(TimeoutException.class, () -> drained.get(50, TimeUnit.MILLISECONDS));
            messages.acknowledge(deliveries.get(1).sequence);
            assertTrue(drained.get(1, TimeUnit.SECONDS));
            messages.add("unfinished");
            var cancelled = executor.submit(messages::awaitDrained);
            messages.close();
            assertFalse(cancelled.get(1, TimeUnit.SECONDS));
        }
    }

    @Test public void callbacksDoNotHoldTheQueueMonitorAndTimeoutDoesNotGrowTheQueue() throws Exception {
        CountDownLatch delivered = new CountDownLatch(1);
        CountDownLatch release = new CountDownLatch(1);
        SocketMessages messages = new SocketMessages(true, batch -> {
            delivered.countDown();
            try { release.await(); } catch (InterruptedException e) { Thread.currentThread().interrupt(); }
        });
        try (var executor = Executors.newFixedThreadPool(2)) {
            var first = executor.submit(() -> messages.add("first"));
            assertTrue(delivered.await(1, TimeUnit.SECONDS));
            try { executor.submit(messages::close).get(1, TimeUnit.SECONDS); }
            finally { release.countDown(); }
            first.get(1, TimeUnit.SECONDS);
        }
        List<SocketMessages.Batch> deliveries = new ArrayList<>();
        SocketMessages bounded = new SocketMessages(true, 1, 4, TimeUnit.MILLISECONDS.toNanos(10), deliveries::add);
        assertTrue(bounded.add("one"));
        assertFalse(bounded.add("two"));
        assertFalse(bounded.awaitDrained());
        bounded.acknowledge(deliveries.get(0).sequence);
        assertTrue(bounded.awaitDrained());
    }
}
