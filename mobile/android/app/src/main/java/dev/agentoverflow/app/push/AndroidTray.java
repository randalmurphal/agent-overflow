package dev.agentoverflow.app.push;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.content.Context;
import android.content.Intent;

import androidx.core.app.NotificationCompat;

import dev.agentoverflow.app.MainActivity;
import dev.agentoverflow.app.R;

/**
 * The Android half of {@link TrayNotifier}: everything that needed a
 * platform type, and nothing that needed a decision.
 *
 * <p>The split is the point. What to post, what to cancel and when to
 * drop one is decided in {@code TrayNotifier}, which a JVM test covers;
 * this file is the builder call and the intent, which only a device can
 * prove. Keeping it this thin is what keeps the untested surface small.
 */
final class AndroidTray implements TrayNotifier.Tray {

    /**
     * The extra the launch intent carries the tap route in.
     *
     * <p>An OPAQUE JSON document, forwarded from the message to the
     * intent to the web layer without being parsed here. The route's
     * field names belong to {@code internal/notify.Target} and to the
     * page's own {@code parseNotificationTarget}; a copy of them in Java
     * would be a third spelling to keep true, and the only thing this
     * side does with the value is carry it.
     */
    static final String EXTRA_TARGET = "dev.agentoverflow.app.push.TARGET";

    /** The extra naming which notification was tapped. */
    static final String EXTRA_ID = "dev.agentoverflow.app.push.ID";

    private final Context context;

    AndroidTray(Context context) {
        this.context = context.getApplicationContext();
    }

    @Override
    public void post(TrayNotifier.Presentation presentation) {
        NotificationManager manager = manager();
        if (manager == null) {
            return;
        }
        ensureChannel(manager);

        Intent tap = new Intent(context, MainActivity.class);
        // The activity is singleTask, so a tap on a running app delivers
        // this through onNewIntent rather than starting a second one.
        tap.setFlags(Intent.FLAG_ACTIVITY_NEW_TASK | Intent.FLAG_ACTIVITY_CLEAR_TOP);
        tap.putExtra(EXTRA_TARGET, presentation.target);
        tap.putExtra(EXTRA_ID, presentation.tag);
        // The tag is the request code, so two live notifications get two
        // distinct PendingIntents. Sharing one would hand the second
        // notification the first one's extras, and the tap would open the
        // wrong thread.
        PendingIntent pending = PendingIntent.getActivity(
                context,
                presentation.tag.hashCode(),
                tap,
                PendingIntent.FLAG_UPDATE_CURRENT | PendingIntent.FLAG_IMMUTABLE);

        Notification notification = new NotificationCompat.Builder(context, TrayNotifier.CHANNEL_ID)
                .setSmallIcon(R.mipmap.ic_launcher)
                .setContentTitle(presentation.title)
                .setContentText(presentation.body)
                .setContentIntent(pending)
                .setAutoCancel(true)
                .setPriority(NotificationCompat.PRIORITY_DEFAULT)
                .build();

        // Tag, not a numeric id: the tag is the send id, which is what
        // makes a later state change REPLACE this notification and a
        // retraction cancel exactly it.
        manager.notify(presentation.tag, 0, notification);
    }

    @Override
    public void cancel(String tag) {
        NotificationManager manager = manager();
        if (manager != null) {
            manager.cancel(tag, 0);
        }
    }

    private NotificationManager manager() {
        return (NotificationManager) context.getSystemService(Context.NOTIFICATION_SERVICE);
    }

    /**
     * One channel, created on demand and idempotent — creating a channel
     * that exists updates its name and changes nothing the person set.
     */
    private static void ensureChannel(NotificationManager manager) {
        NotificationChannel channel = new NotificationChannel(
                TrayNotifier.CHANNEL_ID,
                "Agent Overflow",
                NotificationManager.IMPORTANCE_DEFAULT);
        channel.setShowBadge(true);
        manager.createNotificationChannel(channel);
    }
}
