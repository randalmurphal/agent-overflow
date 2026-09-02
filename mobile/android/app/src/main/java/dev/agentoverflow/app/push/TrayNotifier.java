package dev.agentoverflow.app.push;

import java.util.Map;

/**
 * The one place a push notification is built or cancelled.
 *
 * <p><b>Two callers, one rule.</b> {@link PushMessagingService} calls it
 * for a message that arrived through Google while the app was not
 * looking; {@link PushPlugin} calls it for a `notification:send` that
 * arrived over the app's own socket. If those two built notifications
 * separately they would drift — different channel, different tag, a
 * retraction that cancels an id the other spelled differently — and the
 * symptom would be a notification nobody can dismiss.
 *
 * <p><b>No Android type in this file</b>, deliberately, exactly as
 * {@code BundleStore} has none: the tray itself is behind {@link Tray},
 * so the whole data → notification decision is covered by a plain JVM
 * unit test rather than by an emulator run nobody can perform on the
 * development box. What is left on the other side of that interface is
 * one builder call.
 *
 * <p><b>The data keys are {@code internal/push}'s, mirrored.</b> That
 * package is where they are defined and where the argument for each of
 * them lives; this is the reader. A key added there is added here, and
 * the Go side's tests are what say what may be in them.
 */
final class TrayNotifier {

    /** Mirrors {@code push.KeyID}: the notify.Send id, half of the tray tag. */
    static final String KEY_ID = "id";

    /**
     * Mirrors {@code push.KeyBackend}: the sending backend's identity, the
     * other half of the tray tag. See {@link #tagFor}.
     */
    static final String KEY_BACKEND = "backend";

    /** Mirrors {@code push.KeyKind}. */
    static final String KEY_KIND = "kind";

    /** Mirrors {@code push.KeyRetract}. Present only on a withdrawal. */
    static final String KEY_RETRACT = "retract";

    /** Mirrors {@code push.KeyTitle}: one of six fixed phrases. */
    static final String KEY_TITLE = "title";

    /** Mirrors {@code push.KeyBody}: the backend's display name. */
    static final String KEY_BODY = "body";

    /** Mirrors {@code push.KeyTarget}: the tap route, as a JSON document. */
    static final String KEY_TARGET = "target";

    /** Mirrors {@code push.RetractValue}. */
    static final String RETRACT_VALUE = "1";

    /**
     * The one notification channel. Named rather than per-kind because
     * the kinds are already the user's own toggles on the backend, and a
     * second set of switches in Android settings would be a second
     * answer to one question.
     */
    static final String CHANNEL_ID = "agent-overflow";

    /** The tray, as this class needs it. Implemented by {@link AndroidTray}. */
    interface Tray {
        void post(Presentation presentation);

        void cancel(String tag);
    }

    /** One notification to put on screen. */
    static final class Presentation {
        final String tag;
        final String kind;
        final String title;
        final String body;
        /** The tap route as its own JSON document, forwarded untouched. */
        final String target;

        Presentation(String tag, String kind, String title, String body, String target) {
            this.tag = tag;
            this.kind = kind;
            this.title = title;
            this.body = body;
            this.target = target;
        }
    }

    /**
     * What one message asks for: a presentation, or the withdrawal of
     * one.
     *
     * <p>A retraction carries an id and nothing else it needs, which is
     * the contract {@code internal/notify} states and {@code
     * internal/push} enforces: withdrawing something is not a
     * presentation and has no phrase, no backend name and no route.
     */
    static final class Action {
        final boolean retract;
        final Presentation presentation;
        final String tag;

        private Action(boolean retract, String tag, Presentation presentation) {
            this.retract = retract;
            this.tag = tag;
            this.presentation = presentation;
        }

        static Action retraction(String tag) {
            return new Action(true, tag, null);
        }

        static Action presentation(Presentation presentation) {
            return new Action(false, presentation.tag, presentation);
        }
    }

    /**
     * The tray tag for one moment: {@code <backend>|<id>}, mirroring
     * {@code push.TrayTag} and {@code pushTag} in
     * {@code stores/pushPresenter.svelte.ts}.
     *
     * <p>Namespaced by BACKEND because a phone is paired with several and
     * notification ids are not unique across them: {@code
     * provider-auth:claude} is the same string on every machine the owner
     * runs, and a tag of the id alone would let one machine's sign-out
     * notice silently replace another's. The socket presenter composes
     * the same tag from the frame's origin, so a phone told about one
     * moment by both paths shows ONE notification.
     *
     * <p>A message with no backend keeps the bare id. The backend sends
     * none such, but a retraction from a sender that omitted it omits it
     * too, so the fallback still cancels what it posted.
     */
    static String tagFor(String backend, String id) {
        return backend.isEmpty() ? id : backend + "|" + id;
    }

    /**
     * Read one message's data into an action, or {@code null} when it
     * names nothing this shell can act on.
     *
     * <p>A message with no id is dropped rather than posted under some
     * substitute tag. The id is what makes a later state change REPLACE
     * this notification instead of stacking a second one beside a fact
     * that is no longer true, so a notification without one is worse
     * than no notification: it is one that can never be withdrawn.
     */
    static Action read(Map<String, String> data) {
        if (data == null) {
            return null;
        }
        String id = text(data.get(KEY_ID));
        if (id.isEmpty()) {
            return null;
        }
        String tag = tagFor(text(data.get(KEY_BACKEND)), id);
        if (RETRACT_VALUE.equals(text(data.get(KEY_RETRACT)))) {
            return Action.retraction(tag);
        }
        return Action.presentation(new Presentation(
                tag,
                text(data.get(KEY_KIND)),
                text(data.get(KEY_TITLE)),
                text(data.get(KEY_BODY)),
                text(data.get(KEY_TARGET))));
    }

    private final Tray tray;

    TrayNotifier(Tray tray) {
        this.tray = tray;
    }

    /**
     * Apply one action.
     *
     * <p><b>A presentation is dropped while the app is in the
     * foreground.</b> The socket's own {@code notification:send} is
     * already presenting it there, exactly as it is on the desktop, and
     * posting a tray notification over an app the person is looking at
     * is the double notification this whole seam exists to avoid.
     *
     * <p><b>A retraction is never dropped.</b> The notification being
     * withdrawn was posted while the app was in the background, and the
     * app coming to the foreground does not take it off the tray. Gating
     * the withdrawal on the same flag that gated the posting would leave
     * exactly those notifications on screen forever — the shape of the
     * bug {@code notifyOS} refuses for the same reason.
     */
    void apply(Action action, boolean foreground) {
        if (action == null) {
            return;
        }
        if (action.retract) {
            tray.cancel(action.tag);
            return;
        }
        if (foreground) {
            return;
        }
        tray.post(action.presentation);
    }

    private static String text(String value) {
        return value == null ? "" : value.trim();
    }
}
