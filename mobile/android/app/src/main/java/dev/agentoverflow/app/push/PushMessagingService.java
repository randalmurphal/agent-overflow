package dev.agentoverflow.app.push;

import android.content.Context;
import android.content.SharedPreferences;

import androidx.annotation.NonNull;

import com.google.firebase.messaging.FirebaseMessagingService;
import com.google.firebase.messaging.RemoteMessage;

/**
 * What Google delivers, and what this phone does with it.
 *
 * <p><b>Every message reaches this method</b>, foreground or background,
 * because the backend sends DATA-ONLY messages and never a Google-composed
 * `notification` (see {@code internal/push}). That is what buys the two
 * behaviours below: a notification this app is already showing in its own
 * UI can be DROPPED, and one already on the tray can be CANCELLED. Neither
 * is possible for a Google-composed notification, which the system posts
 * before any of our code runs — and cancelling is half of this feature.
 *
 * <p>It is also why {@code @capacitor/push-notifications} is not used: it
 * builds tray notifications only for the composed kind and cannot cancel
 * one.
 *
 * <p><b>This app declares no other messaging service.</b> Android
 * dispatches {@code MESSAGING_EVENT} to the first match, so a second
 * declaration anywhere in the merged manifest would silently take the
 * messages instead.
 */
public class PushMessagingService extends FirebaseMessagingService {

    /** Where the last known registration token is kept between launches. */
    static final String PREFS = "dev.agentoverflow.app.push";

    /** The key inside {@link #PREFS}. */
    static final String KEY_TOKEN = "token";

    @Override
    public void onMessageReceived(@NonNull RemoteMessage message) {
        TrayNotifier.Action action = TrayNotifier.read(message.getData());
        new TrayNotifier(new AndroidTray(this)).apply(action, PushPlugin.appIsForeground());
    }

    /**
     * A new registration token, which happens on the first launch after
     * install and whenever the platform rotates one (a restore to a new
     * device, a reinstall, cleared app data).
     *
     * <p>Stored FIRST, then offered to the bridge. The store is what the
     * next launch reads if the bridge is not up right now — this callback
     * can arrive with no activity at all — and a token the backend never
     * hears about is a phone that silently stops being woken.
     */
    @Override
    public void onNewToken(@NonNull String token) {
        rememberToken(this, token);
        PushPlugin.deliverRefreshedToken(token);
    }

    static void rememberToken(Context context, String token) {
        prefs(context).edit().putString(KEY_TOKEN, token).apply();
    }

    static String rememberedToken(Context context) {
        return prefs(context).getString(KEY_TOKEN, "");
    }

    private static SharedPreferences prefs(Context context) {
        return context.getApplicationContext().getSharedPreferences(PREFS, Context.MODE_PRIVATE);
    }
}
