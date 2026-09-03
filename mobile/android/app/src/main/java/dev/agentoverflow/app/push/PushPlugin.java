package dev.agentoverflow.app.push;

import android.Manifest;
import android.content.Intent;
import android.os.Build;

import com.getcapacitor.JSObject;
import com.getcapacitor.PermissionState;
import com.getcapacitor.Plugin;
import com.getcapacitor.PluginCall;
import com.getcapacitor.PluginMethod;
import com.getcapacitor.annotation.CapacitorPlugin;
import com.getcapacitor.annotation.Permission;
import com.getcapacitor.annotation.PermissionCallback;
import com.google.firebase.FirebaseApp;
import com.google.firebase.messaging.FirebaseMessaging;

import java.util.concurrent.atomic.AtomicBoolean;

/**
 * The five calls {@code frontend/src/lib/native/push.ts} and {@code
 * stores/pushPresenter.svelte.ts} make, plus the tap event.
 *
 * <p>Ours rather than {@code @capacitor/push-notifications}, for the
 * reason {@link PushMessagingService} states: that plugin renders only
 * Google-composed notifications and cannot cancel one, and cancelling is
 * half of this feature.
 *
 * <p><b>A build with no {@code google-services.json} still works.</b>
 * Firebase self-initialises from resources that file generates, so
 * without it there is no default {@code FirebaseApp} and every messaging
 * call would throw. {@link #getToken} therefore answers a TYPED "not
 * configured" instead, and the web seam reads that as "this phone cannot
 * be woken" and stops — no error banner, no retry loop. That is the
 * state of the development box, and it is a state an APK is allowed to
 * be in.
 */
@CapacitorPlugin(
        name = "Push",
        permissions = {
            @Permission(strings = {Manifest.permission.POST_NOTIFICATIONS}, alias = PushPlugin.NOTIFICATIONS)
        })
public class PushPlugin extends Plugin {

    static final String NOTIFICATIONS = "notifications";

    /**
     * Whether this app is on screen right now.
     *
     * <p>Read by {@link PushMessagingService}, which runs on its own
     * thread with no activity of its own, to decide whether to drop a
     * presentation the app's own socket is already showing.
     *
     * <p>Static and set from the plugin's own resume/pause hooks rather
     * than through {@code androidx.lifecycle:lifecycle-process}. Same
     * answer, one fewer dependency, and it fails in the right direction:
     * a process that was killed comes back with this false, so a message
     * arriving before the app is up is POSTED rather than silently
     * dropped.
     */
    private static final AtomicBoolean FOREGROUND = new AtomicBoolean(false);

    /**
     * The live instance, for {@link PushMessagingService#onNewToken} to
     * reach. Null whenever the bridge is not up, which is normal: the
     * token is stored before this is consulted, and the next launch reads
     * it from there.
     */
    private static volatile PushPlugin live;

    static boolean appIsForeground() {
        return FOREGROUND.get();
    }

    /**
     * Offer a rotated token to the page, if there is a page.
     *
     * <p>Fire and forget by design. {@link PushMessagingService} has
     * already written it down, so a phone whose app is not running
     * registers the new token on its next launch instead.
     */
    static void deliverRefreshedToken(String token) {
        PushPlugin plugin = live;
        if (plugin == null) {
            return;
        }
        JSObject payload = new JSObject();
        payload.put("token", token);
        plugin.notifyListeners("tokenRefresh", payload);
    }

    @Override
    public void load() {
        live = this;
        // The cold-start intent. A tap on a notification while the app is
        // DEAD launches the activity with the extras attached, and there
        // is no onNewIntent for that case — reading only that one would
        // lose every tap on a phone that had been idle, which is most of
        // them.
        pendingTap = tapFrom(getActivity() == null ? null : getActivity().getIntent());
    }

    @Override
    protected void handleOnDestroy() {
        if (live == this) {
            live = null;
        }
        FOREGROUND.set(false);
    }

    @Override
    protected void handleOnResume() {
        FOREGROUND.set(true);
    }

    @Override
    protected void handleOnPause() {
        FOREGROUND.set(false);
    }

    @Override
    protected void handleOnNewIntent(Intent intent) {
        JSObject tap = tapFrom(intent);
        if (tap != null) {
            notifyListeners("tap", tap);
        }
    }

    /**
     * A tap that arrived before the web layer was listening.
     *
     * <p>Held rather than emitted, because a cold start reaches {@link
     * #load} long before the page has added its listener, and an event
     * fired into nobody is a tap that opened nothing. The page collects
     * it with {@link #takePendingTap} as its first act.
     */
    private JSObject pendingTap;

    /** The permission prompt, on the versions that have one. */
    @PluginMethod
    public void requestPermission(PluginCall call) {
        // Below API 33 posting a notification needs no runtime grant, so
        // the honest answer is "granted" rather than a prompt that would
        // never appear.
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) {
            call.resolve(granted(true));
            return;
        }
        if (getPermissionState(NOTIFICATIONS) == PermissionState.GRANTED) {
            call.resolve(granted(true));
            return;
        }
        requestPermissionForAlias(NOTIFICATIONS, call, "permissionResult");
    }

    @PermissionCallback
    private void permissionResult(PluginCall call) {
        call.resolve(granted(getPermissionState(NOTIFICATIONS) == PermissionState.GRANTED));
    }

    /**
     * This device's registration token.
     *
     * <p>Answers {@code {configured: false, token: ""}} when Firebase is
     * not initialised, which is a fact about the build rather than a
     * failure of this call — hence a resolve and not a reject. The web
     * seam stops there and never asks again this launch.
     */
    @PluginMethod
    public void getToken(PluginCall call) {
        if (FirebaseApp.getApps(getContext()).isEmpty()) {
            JSObject answer = new JSObject();
            answer.put("configured", false);
            answer.put("token", "");
            call.resolve(answer);
            return;
        }
        FirebaseMessaging.getInstance()
                .getToken()
                .addOnCompleteListener(task -> {
                    if (!task.isSuccessful()) {
                        Exception cause = task.getException();
                        call.reject(cause == null ? "no registration token" : cause.getMessage());
                        return;
                    }
                    String token = task.getResult();
                    PushMessagingService.rememberToken(getContext(), token);
                    JSObject answer = new JSObject();
                    answer.put("configured", true);
                    answer.put("token", token);
                    call.resolve(answer);
                });
    }

    /**
     * Present a notification the app's own socket delivered.
     *
     * <p>Through the SAME {@link TrayNotifier} the pushed path uses, so
     * the two cannot drift apart on channel, tag, or what a retraction
     * cancels. The foreground flag is passed as false because the caller
     * has already decided: the page only calls this while its lease says
     * background.
     *
     * <p>This content came over the paired session and not through
     * Google, so it may carry the thread title the pushed payload may
     * not. That difference is the whole reason both paths exist.
     */
    @PluginMethod
    public void present(PluginCall call) {
        String id = call.getString("id", "");
        if (id == null || id.isEmpty()) {
            call.reject("a notification needs an id");
            return;
        }
        notifier().apply(
                TrayNotifier.Action.presentation(new TrayNotifier.Presentation(
                        id,
                        call.getString("kind", ""),
                        call.getString("title", ""),
                        call.getString("body", ""),
                        call.getString("target", ""))),
                false);
        call.resolve();
    }

    /** Withdraw one by id. */
    @PluginMethod
    public void retract(PluginCall call) {
        String id = call.getString("id", "");
        if (id == null || id.isEmpty()) {
            call.reject("a retraction needs an id");
            return;
        }
        notifier().apply(TrayNotifier.Action.retraction(id), false);
        call.resolve();
    }

    /**
     * The tap this launch started with, once. Answers an empty object
     * when the app was opened normally.
     */
    @PluginMethod
    public void takePendingTap(PluginCall call) {
        JSObject tap = pendingTap;
        pendingTap = null;
        call.resolve(tap == null ? new JSObject() : tap);
    }

    private TrayNotifier notifier() {
        return new TrayNotifier(new AndroidTray(getContext()));
    }

    private static JSObject granted(boolean value) {
        JSObject answer = new JSObject();
        answer.put("granted", value);
        return answer;
    }

    /**
     * The tap payload an intent carries, or null when it carries none.
     * The target rides through untouched: its field names belong to the
     * Go and page halves, not to this one.
     */
    private static JSObject tapFrom(Intent intent) {
        if (intent == null) {
            return null;
        }
        String target = intent.getStringExtra(AndroidTray.EXTRA_TARGET);
        if (target == null || target.isEmpty()) {
            return null;
        }
        JSObject tap = new JSObject();
        tap.put("id", intent.getStringExtra(AndroidTray.EXTRA_ID));
        tap.put("target", target);
        return tap;
    }
}
