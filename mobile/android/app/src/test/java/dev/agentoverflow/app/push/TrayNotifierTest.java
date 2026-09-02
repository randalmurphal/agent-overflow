package dev.agentoverflow.app.push;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;

import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

import org.junit.Test;

/**
 * The data → notification decision, on the JVM.
 *
 * <p>This is the point of {@link TrayNotifier} taking a {@link
 * TrayNotifier.Tray} and no Android type: which message becomes a
 * notification, which one cancels one, and which one is dropped is the
 * part of push a person notices being wrong, and none of it should need
 * an emulator to be proved. What is left for a device is the part only a
 * device has — that the builder really posts, and that the intent really
 * reaches the activity.
 */
public class TrayNotifierTest {

    /** Records what the tray was asked to do, in order. */
    private static final class RecordingTray implements TrayNotifier.Tray {
        final List<String> calls = new ArrayList<>();
        final List<TrayNotifier.Presentation> posted = new ArrayList<>();

        @Override
        public void post(TrayNotifier.Presentation presentation) {
            calls.add("post:" + presentation.tag);
            posted.add(presentation);
        }

        @Override
        public void cancel(String tag) {
            calls.add("cancel:" + tag);
        }
    }

    private static Map<String, String> presentationData() {
        Map<String, String> data = new HashMap<>();
        data.put(TrayNotifier.KEY_ID, "thread:t-1");
        data.put(TrayNotifier.KEY_KIND, "turn-complete");
        data.put(TrayNotifier.KEY_TITLE, "Turn complete");
        data.put(TrayNotifier.KEY_BODY, "workshop");
        data.put(TrayNotifier.KEY_TARGET, "{\"kind\":\"thread\",\"threadId\":\"t-1\"}");
        return data;
    }

    @Test
    public void aMessageBecomesOneNotificationTaggedWithItsId() {
        RecordingTray tray = new RecordingTray();
        new TrayNotifier(tray).apply(TrayNotifier.read(presentationData()), false);

        assertEquals(List.of("post:thread:t-1"), tray.calls);
        TrayNotifier.Presentation posted = tray.posted.get(0);
        assertEquals("Turn complete", posted.title);
        assertEquals("workshop", posted.body);
        assertEquals("turn-complete", posted.kind);
        // The tag IS the id. That is what makes a later state change
        // replace this notification rather than stack a second one beside
        // a fact that is no longer true.
        assertEquals("thread:t-1", posted.tag);
    }

    @Test
    public void theTargetRidesThroughUntouched() {
        RecordingTray tray = new RecordingTray();
        new TrayNotifier(tray).apply(TrayNotifier.read(presentationData()), false);
        // Not parsed here, and deliberately not: the route's field names
        // belong to the Go side and to the page, and a third spelling in
        // Java would be a third thing to keep true.
        assertEquals("{\"kind\":\"thread\",\"threadId\":\"t-1\"}", tray.posted.get(0).target);
    }

    @Test
    public void aRetractionCancelsByTagAndCarriesNothingElse() {
        Map<String, String> data = new HashMap<>();
        data.put(TrayNotifier.KEY_ID, "thread:t-1");
        data.put(TrayNotifier.KEY_KIND, "turn-complete");
        data.put(TrayNotifier.KEY_RETRACT, TrayNotifier.RETRACT_VALUE);

        TrayNotifier.Action action = TrayNotifier.read(data);
        assertTrue(action.retract);
        assertNull(action.presentation);

        RecordingTray tray = new RecordingTray();
        new TrayNotifier(tray).apply(action, false);
        assertEquals(List.of("cancel:thread:t-1"), tray.calls);
    }

    /**
     * The foreground drop. The app's own socket is already showing this
     * one, exactly as on the desktop, and posting a tray notification
     * over an app the person is looking at is the double notification
     * the whole seam exists to avoid.
     */
    @Test
    public void aPresentationIsDroppedWhileTheAppIsOnScreen() {
        RecordingTray tray = new RecordingTray();
        new TrayNotifier(tray).apply(TrayNotifier.read(presentationData()), true);
        assertTrue("a notification was posted over the app the person is looking at", tray.calls.isEmpty());
    }

    /**
     * A RETRACTION IS NEVER DROPPED, and this is the case that matters:
     * the notification was posted while the app was in the background,
     * and the app coming to the foreground does not take it off the tray.
     * Gating the withdrawal on the flag that gated the posting would
     * strand exactly those notifications forever.
     */
    @Test
    public void aRetractionStillCancelsWhileTheAppIsOnScreen() {
        Map<String, String> data = new HashMap<>();
        data.put(TrayNotifier.KEY_ID, "thread:t-1");
        data.put(TrayNotifier.KEY_RETRACT, TrayNotifier.RETRACT_VALUE);

        RecordingTray tray = new RecordingTray();
        new TrayNotifier(tray).apply(TrayNotifier.read(data), true);
        assertEquals(List.of("cancel:thread:t-1"), tray.calls);
    }

    /**
     * A message with no id is dropped rather than posted under some
     * substitute tag: a notification that can never be withdrawn is worse
     * than no notification.
     */
    @Test
    public void aMessageWithNoIdIsNotANotification() {
        Map<String, String> data = new HashMap<>();
        data.put(TrayNotifier.KEY_TITLE, "Turn complete");
        assertNull(TrayNotifier.read(data));

        RecordingTray tray = new RecordingTray();
        new TrayNotifier(tray).apply(TrayNotifier.read(data), false);
        new TrayNotifier(tray).apply(TrayNotifier.read(null), false);
        assertTrue(tray.calls.isEmpty());
    }

    /**
     * A message that names only some of the presentation keys still
     * posts. The backend composes all of them, so this is not a shape it
     * sends — but an older backend that did not is a phone that should
     * still buzz, not one that silently shows nothing.
     */
    @Test
    public void aSparseMessageStillPostsWithEmptyText() {
        Map<String, String> data = new HashMap<>();
        data.put(TrayNotifier.KEY_ID, "approval:t-1:r-9");

        RecordingTray tray = new RecordingTray();
        new TrayNotifier(tray).apply(TrayNotifier.read(data), false);
        assertEquals(List.of("post:approval:t-1:r-9"), tray.calls);
        assertEquals("", tray.posted.get(0).title);
        assertEquals("", tray.posted.get(0).target);
    }
}
