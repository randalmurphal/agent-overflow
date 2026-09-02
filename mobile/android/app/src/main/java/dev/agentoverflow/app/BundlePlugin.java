package dev.agentoverflow.app;

import android.content.pm.PackageInfo;
import android.content.pm.PackageManager;
import android.os.Build;

import com.getcapacitor.JSArray;
import com.getcapacitor.JSObject;
import com.getcapacitor.Plugin;
import com.getcapacitor.PluginCall;
import com.getcapacitor.PluginMethod;
import com.getcapacitor.annotation.CapacitorPlugin;

import java.io.File;
import java.util.Base64;
import java.util.Map;

/**
 * The three calls {@code frontend/src/lib/native/bundleSync.ts} makes.
 *
 * <p>A plugin local to this app rather than a published package, because
 * what it does is specific to this backend's update channel and there is
 * nothing here anyone else would install. It is the only native code
 * wave 6g-a adds.
 *
 * <p>Everything it decides lives in {@link BundleStore}, which takes a
 * directory and no Android type, so the whole mechanic is covered by a
 * plain JVM unit test. What is left here is the bridge: decode the
 * arguments, name the failure, and answer.
 *
 * <p><b>Failures reject with the message the store produced.</b> That
 * message names the file, and the shell logs it and retries later. A
 * generic "staging failed" would leave a person with a phone that never
 * updates and no way to find out why.
 */
@CapacitorPlugin(name = "Bundle")
public class BundlePlugin extends Plugin {

    /**
     * The directory every bundle lives under, relative to {@code
     * filesDir}. Named here and in {@code MainActivity} through the same
     * helper so the boot path and the plugin can never address different
     * trees.
     */
    static File rootFor(File filesDir) {
        return new File(filesDir, "bundles");
    }

    private BundleStore store() {
        return new BundleStore(rootFor(getContext().getFilesDir()));
    }

    /**
     * Unzip, verify and adopt one downloaded bundle as {@code next}.
     *
     * <p>The archive arrives base64-encoded because that is what a
     * Capacitor call can carry: the bridge marshals JSON. It is a few MB
     * once per update, on a background thread, so the inflation is paid
     * where nothing is waiting on it.
     */
    @PluginMethod
    public void stage(PluginCall call) {
        String id = call.getString("id", "");
        JSObject manifest = call.getObject("manifest");
        String archiveBase64 = call.getString("archiveBase64", "");
        if (id == null || id.isEmpty()) {
            call.reject("a bundle needs an id");
            return;
        }
        if (manifest == null) {
            call.reject("bundle " + id + " arrived with no manifest");
            return;
        }
        if (archiveBase64 == null || archiveBase64.isEmpty()) {
            call.reject("bundle " + id + " arrived with no archive");
            return;
        }
        byte[] archive;
        try {
            archive = Base64.getDecoder().decode(archiveBase64);
        } catch (IllegalArgumentException notBase64) {
            call.reject("bundle " + id + " did not decode: " + notBase64.getMessage());
            return;
        }
        try {
            // JSObject IS a JSONObject; the store reads the plain shape so
            // it stays testable without a Capacitor type on the classpath.
            Map<String, BundleStore.FileSpec> files = BundleStore.readManifest(manifest);
            store().stage(id, files, archive);
        } catch (BundleStore.BundleException refused) {
            call.reject(refused.getMessage());
            return;
        }
        call.resolve();
    }

    /**
     * The health check. Called once per launch by the shell, after the
     * app has mounted and its boot has run to the end — getting as far
     * as this call IS the check, because a bundle that loads, renders
     * and reaches its plugin is a bundle that works. Reaching the backend
     * is deliberately not part of it: a phone launched offline must not
     * roll back a good bundle.
     */
    @PluginMethod
    public void ready(PluginCall call) {
        store().ready();
        call.resolve();
    }

    /**
     * What this device is running, plus the APK's own {@code
     * versionCode}.
     *
     * <p>The version code is read here rather than through
     * {@code @capacitor/app} so the seam has exactly one native
     * dependency. The shell compares it against the bundle's
     * {@code minShellBuild} before it downloads anything, and a seam that
     * needed a second plugin to answer that question would be a second
     * plugin the update could not ship.
     */
    @PluginMethod
    public void state(PluginCall call) {
        BundleStore.State state = store().read();
        JSObject answer = new JSObject();
        answer.put("current", state.current);
        answer.put("next", state.next);
        answer.put("pendingHealth", state.pendingHealth);
        answer.put("lastKnownGood", state.lastKnownGood);
        answer.put("rolledBack", new JSArray(state.rolledBack));
        answer.put("versionCode", versionCode());
        call.resolve(answer);
    }

    /**
     * This build's {@code versionCode}. Zero when the platform cannot
     * answer, which the shell reads as "below every floor" and therefore
     * declines to download — the safe direction: a phone that cannot say
     * what it is does not get a bundle it might not be able to run.
     */
    @SuppressWarnings("deprecation")
    private long versionCode() {
        try {
            PackageManager packages = getContext().getPackageManager();
            PackageInfo info = packages.getPackageInfo(getContext().getPackageName(), 0);
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) {
                return info.getLongVersionCode();
            }
            return info.versionCode;
        } catch (PackageManager.NameNotFoundException | RuntimeException unavailable) {
            return 0L;
        }
    }
}
