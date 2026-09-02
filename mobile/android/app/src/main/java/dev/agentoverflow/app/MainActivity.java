package dev.agentoverflow.app;

import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;

import com.getcapacitor.BridgeActivity;
import com.getcapacitor.ServerPath;

import java.io.File;

/**
 * The shell's activity, and the one place that decides which web bundle
 * this launch runs.
 *
 * <p><b>The decision happens BEFORE {@code super.onCreate}</b>, and it has
 * to. {@code BridgeActivity.onCreate} builds the Bridge from
 * {@code bridgeBuilder} and immediately loads the WebView from whatever
 * server path that builder holds, so a path installed afterwards would
 * mean one launch of the wrong bundle every time. {@code registerPlugin}
 * is before it for the same reason: the builder collects plugins and the
 * Bridge is created from it once.
 *
 * <p><b>The 30-second watchdog is the other half of the health check.</b>
 * The shell calls {@code Bundle.ready()} once its app has mounted and its
 * boot has run to the end. A bundle that hangs before then would
 * otherwise sit on a dead screen until the person killed the app — and
 * killing it is what ARMS the rollback, so they would have to work out
 * that killing it is the fix. Instead, if the health flag is still set
 * when the timer fires, the rollback runs in place and the WebView is
 * pointed back at the last known good bundle (or at the APK's own
 * assets), without a restart.
 */
public class MainActivity extends BridgeActivity {

    /**
     * How long a fresh bundle has to say it is working.
     *
     * <p>Generous on purpose: it has to cover a cold WebView start and
     * the app's whole boot fan-out on a slow phone, and the cost of being
     * wrong in the slow direction is a rollback nobody needed. Thirty
     * seconds is far past a healthy boot and far short of a person's
     * patience with a blank screen.
     */
    private static final long HEALTH_DEADLINE_MS = 30_000L;

    private final Handler watchdog = new Handler(Looper.getMainLooper());
    private BundleStore bundles;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        registerPlugin(BundlePlugin.class);

        bundles = new BundleStore(BundlePlugin.rootFor(getFilesDir()));
        File serving = bundles.onBoot();
        if (serving != null) {
            bridgeBuilder.setServerPath(
                    new ServerPath(ServerPath.PathType.BASE_PATH, serving.getAbsolutePath()));
        }

        super.onCreate(savedInstanceState);

        if (bundles.awaitingHealth()) {
            watchdog.postDelayed(this::rollBackIfStillUnhealthy, HEALTH_DEADLINE_MS);
        }
    }

    @Override
    public void onDestroy() {
        watchdog.removeCallbacksAndMessages(null);
        super.onDestroy();
    }

    /**
     * The deadline fired. If nothing has reported healthy, roll back and
     * reload onto the fallback in place.
     *
     * <p>{@code setServerBasePath} / {@code setServerAssetPath} both host
     * the new path and re-load the WebView, which is exactly the recovery
     * wanted: the person sees the app they had, not a restart they had to
     * think of.
     */
    private void rollBackIfStillUnhealthy() {
        if (bundles == null || !bundles.awaitingHealth()) {
            return;
        }
        File fallback = bundles.rollbackUnhealthy();
        if (bridge == null) {
            return;
        }
        if (fallback != null) {
            bridge.setServerBasePath(fallback.getAbsolutePath());
            return;
        }
        bridge.setServerAssetPath(com.getcapacitor.Bridge.DEFAULT_WEB_ASSET_DIR);
    }
}
