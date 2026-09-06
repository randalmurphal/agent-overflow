package dev.agentoverflow.app;

import static org.junit.Assert.assertArrayEquals;
import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;
import static org.junit.Assert.fail;

import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.security.MessageDigest;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.zip.ZipEntry;
import java.util.zip.ZipOutputStream;

import org.json.JSONArray;
import org.json.JSONObject;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;

/**
 * The whole update mechanic, on the JVM.
 *
 * <p>This is the point of {@link BundleStore} taking a directory and no
 * Android type: the state transitions, the unzip, the verification and
 * the rollback are the part of wave 6g-a that decides whether a phone
 * boots, and none of it should need an emulator to be proved. What is
 * left for the emulator is the part only a device has — that the WebView
 * really serves from the staged directory, and that the plugin registers.
 */
public class BundleStoreTest {

    @Rule
    public final TemporaryFolder temp = new TemporaryFolder();

    private File root() throws IOException {
        return new File(temp.getRoot(), "bundles");
    }

    // -----------------------------------------------------------------
    // Building a bundle to stage
    // -----------------------------------------------------------------

    /** One file's bytes, keyed by its manifest path. */
    private static Map<String, byte[]> spa() {
        Map<String, byte[]> files = new LinkedHashMap<>();
        files.put("index.html", "<!doctype html><title>app</title>".getBytes(StandardCharsets.UTF_8));
        files.put("assets/app.js", "export const a = 1;\n".getBytes(StandardCharsets.UTF_8));
        return files;
    }

    private static String hex(byte[] bytes) {
        StringBuilder out = new StringBuilder(bytes.length * 2);
        for (byte b : bytes) {
            out.append(String.format(Locale.ROOT, "%02x", b));
        }
        return out.toString();
    }

    private static String sha256(byte[] bytes) throws Exception {
        return hex(MessageDigest.getInstance("SHA-256").digest(bytes));
    }

    /** The manifest document the backend serves, over the given files. */
    private static JSONObject manifestFor(Map<String, byte[]> files) throws Exception {
        JSONArray entries = new JSONArray();
        for (Map.Entry<String, byte[]> file : files.entrySet()) {
            JSONObject entry = new JSONObject();
            entry.put("path", file.getKey());
            entry.put("sha256", sha256(file.getValue()));
            entry.put("size", file.getValue().length);
            entries.put(entry);
        }
        JSONObject manifest = new JSONObject();
        manifest.put("id", "bundle-under-test");
        manifest.put("files", entries);
        return manifest;
    }

    /** The archive, in the shape internal/bundle writes it. */
    private static byte[] archiveFor(Map<String, byte[]> files) throws IOException {
        ByteArrayOutputStream buffer = new ByteArrayOutputStream();
        try (ZipOutputStream zip = new ZipOutputStream(buffer)) {
            for (Map.Entry<String, byte[]> file : files.entrySet()) {
                zip.putNextEntry(new ZipEntry(file.getKey()));
                zip.write(file.getValue());
                zip.closeEntry();
            }
        }
        return buffer.toByteArray();
    }

    private void stageOk(BundleStore store, String id, Map<String, byte[]> files) throws Exception {
        // Staging runs only after MainActivity has selected this APK's boot.
        if (store.read().apkBuild == 0) store.onBoot(1);
        store.stage(id, BundleStore.readManifest(manifestFor(files)), archiveFor(files));
    }

    /** Stage and expect a refusal; answers the message. */
    private String stageRefused(
            BundleStore store, String id, JSONObject manifest, byte[] archive) throws Exception {
        try {
            store.stage(id, BundleStore.readManifest(manifest), archive);
        } catch (BundleStore.BundleException refused) {
            return refused.getMessage();
        }
        fail("staging was accepted and should have been refused");
        return "";
    }

    // -----------------------------------------------------------------
    // Staging
    // -----------------------------------------------------------------

    @Test
    public void stagingWritesEveryFileAndArmsTheSwap() throws Exception {
        BundleStore store = new BundleStore(root());
        Map<String, byte[]> files = spa();
        stageOk(store, "abc123", files);

        BundleStore.State state = store.read();
        assertEquals("a staged bundle is next, never current", "abc123", state.next);
        assertEquals("nothing swaps until the next cold start", "", state.current);
        for (Map.Entry<String, byte[]> file : files.entrySet()) {
            File written = new File(store.dir("abc123"), file.getKey());
            assertTrue(file.getKey() + " was not written", written.isFile());
            assertArrayEquals(file.getValue(), Files.readAllBytes(written.toPath()));
        }
        assertFalse(
                "the staging directory must not survive a success",
                new File(root(), "abc123" + BundleStore.STAGING_SUFFIX).exists());
    }

    @Test
    public void aCorruptedFileIsRefusedByName() throws Exception {
        BundleStore store = new BundleStore(root());
        Map<String, byte[]> files = spa();
        JSONObject manifest = manifestFor(files);
        // One byte different from what the manifest promised: the case a
        // damaged transfer or a substituted file produces.
        files.put("assets/app.js", "export const a = 2;\n".getBytes(StandardCharsets.UTF_8));

        String message = stageRefused(store, "abc123", manifest, archiveFor(files));
        assertTrue("the refusal must name the file, got: " + message,
                message.contains("assets/app.js"));
        assertFalse("a refused bundle must leave nothing behind", store.dir("abc123").exists());
        assertFalse(new File(root(), "abc123" + BundleStore.STAGING_SUFFIX).exists());
        assertEquals("a refused bundle must not become next", "", store.read().next);
    }

    @Test
    public void aTruncatedFileIsRefused() throws Exception {
        BundleStore store = new BundleStore(root());
        Map<String, byte[]> files = spa();
        JSONObject manifest = manifestFor(files);
        files.put("assets/app.js", "short".getBytes(StandardCharsets.UTF_8));

        String message = stageRefused(store, "abc123", manifest, archiveFor(files));
        assertTrue("the refusal must name the file, got: " + message,
                message.contains("assets/app.js"));
    }

    @Test
    public void anEntryTheManifestDoesNotNameIsRefused() throws Exception {
        BundleStore store = new BundleStore(root());
        Map<String, byte[]> files = spa();
        JSONObject manifest = manifestFor(files);
        files.put("assets/extra.js", "console.log(1)".getBytes(StandardCharsets.UTF_8));

        String message = stageRefused(store, "abc123", manifest, archiveFor(files));
        assertTrue("the refusal must name the file, got: " + message,
                message.contains("assets/extra.js"));
        assertFalse(store.dir("abc123").exists());
    }

    @Test
    public void aManifestPathTheArchiveNeverCarriedIsRefused() throws Exception {
        BundleStore store = new BundleStore(root());
        Map<String, byte[]> files = spa();
        JSONObject manifest = manifestFor(files);
        Map<String, byte[]> short_ = new LinkedHashMap<>(files);
        short_.remove("assets/app.js");

        String message = stageRefused(store, "abc123", manifest, archiveFor(short_));
        assertTrue("the refusal must name the missing file, got: " + message,
                message.contains("assets/app.js"));
        assertFalse(store.dir("abc123").exists());
    }

    @Test
    public void anEscapingPathIsRefusedWhereTheManifestIsRead() throws Exception {
        BundleStore store = new BundleStore(root());
        JSONObject manifest = manifestFor(spa());
        JSONObject escaping = new JSONObject();
        escaping.put("path", "../outside.js");
        escaping.put("sha256", sha256(new byte[0]));
        escaping.put("size", 0);
        manifest.getJSONArray("files").put(escaping);

        try {
            BundleStore.readManifest(manifest);
            fail("a manifest naming a path outside the bundle was accepted");
        } catch (BundleStore.BundleException refused) {
            assertTrue(refused.getMessage(), refused.getMessage().contains("../outside.js"));
        }
        assertFalse(store.dir("abc123").exists());
    }

    @Test
    public void safePathRefusesEverythingThatEscapes() {
        for (String ok : new String[] {"index.html", "assets/app.js", "a/b/c.txt"}) {
            assertTrue(ok, BundleStore.safePath(ok));
        }
        for (String bad : new String[] {
                "", "/etc/passwd", "../outside", "assets/../../outside",
                "assets/./app.js", "assets//app.js", "assets\\app.js", "C:/windows",
        }) {
            assertFalse(bad, BundleStore.safePath(bad));
        }
    }

    @Test
    public void anArchiveThatIsNotAZipIsRefused() throws Exception {
        BundleStore store = new BundleStore(root());
        String message = stageRefused(
                store, "abc123", manifestFor(spa()), "not a zip".getBytes(StandardCharsets.UTF_8));
        assertFalse(message.isEmpty());
        assertFalse(store.dir("abc123").exists());
    }

    // -----------------------------------------------------------------
    // Boot transitions
    // -----------------------------------------------------------------

    @Test
    public void aFreshInstallServesTheApkAssets() throws Exception {
        BundleStore store = new BundleStore(root());
        assertNull("no state means the APK's own assets", store.onBoot(1));
        assertEquals("", store.read().current);
        assertEquals(1, store.read().apkBuild);
    }

    @Test
    public void anApkUpgradeReplacesCachedAndStagedCodeWithoutTouchingOtherData() throws Exception {
        BundleStore store = new BundleStore(root());
        stageOk(store, "old", spa());
        store.onBoot(1);
        store.ready();
        stageOk(store, "waiting", spa());
        File outsideBundles = new File(temp.getRoot(), "frontend-data");
        Files.write(outsideBundles.toPath(), "keep".getBytes(StandardCharsets.UTF_8));

        assertNull("the newly installed APK owns the first launch", store.onBoot(2));
        BundleStore.State upgraded = store.read();
        assertEquals(2, upgraded.apkBuild);
        assertEquals("", upgraded.current);
        assertEquals("", upgraded.next);
        assertEquals("", upgraded.pendingHealth);
        assertEquals("", upgraded.lastKnownGood);
        assertTrue(upgraded.rolledBack.isEmpty());
        store.ready();
        assertFalse(store.dir("old").exists());
        assertFalse(store.dir("waiting").exists());
        assertEquals("keep", new String(Files.readAllBytes(outsideBundles.toPath()), StandardCharsets.UTF_8));

        stageOk(store, "new", spa());
        assertEquals(store.dir("new"), store.onBoot(2));
        store.ready();
        assertEquals("ordinary restarts retain subsequent web updates",
                store.dir("new"), store.onBoot(2));
    }

    @Test
    public void aLegacyCacheCannotMaskTheFirstApkWithBuildTracking() throws Exception {
        BundleStore store = new BundleStore(root());
        stageOk(store, "legacy", spa());
        store.onBoot(1);
        store.ready();
        JSONObject legacy = store.read().toJson();
        legacy.remove("apkBuild");
        Files.write(new File(root(), "state.json").toPath(), legacy.toString().getBytes(StandardCharsets.UTF_8));

        assertNull(store.onBoot(4));
        assertEquals(4, store.read().apkBuild);
        assertEquals("", store.read().current);
        assertNull("the migration is durable across a cold restart",
                new BundleStore(root()).onBoot(4));
    }

    @Test
    public void anApkUpgradeCannotRollBackIntoThePreviousShellsCode() throws Exception {
        BundleStore store = new BundleStore(root());
        stageOk(store, "good", spa());
        store.onBoot(1);
        store.ready();
        stageOk(store, "unhealthy", spa());
        store.onBoot(1);
        assertTrue(store.awaitingHealth());

        assertNull(store.onBoot(2));
        assertFalse(store.awaitingHealth());
        assertEquals("", store.read().lastKnownGood);
        assertTrue(store.read().rolledBack.isEmpty());
        assertNull(store.onBoot(2));
    }

    @Test
    public void anUnavailableApkVersionPreservesTheLastWorkingBundle() throws Exception {
        BundleStore store = new BundleStore(root());
        stageOk(store, "good", spa());
        store.onBoot(1);
        store.ready();

        assertEquals(store.dir("good"), store.onBoot(0));
        assertEquals(1, store.read().apkBuild);
    }

    @Test
    public void theNextBundleIsAdoptedOnTheFollowingBoot() throws Exception {
        BundleStore store = new BundleStore(root());
        stageOk(store, "abc123", spa());

        File serving = store.onBoot(1);
        assertNotNull("a staged bundle must be served after a cold start", serving);
        assertEquals(store.dir("abc123"), serving);

        BundleStore.State state = store.read();
        assertEquals("abc123", state.current);
        assertEquals("", state.next);
        assertEquals("the first boot on a bundle is on probation", "abc123", state.pendingHealth);
    }

    @Test
    public void aHealthyBootPromotesTheBundleAndReapsTheRest() throws Exception {
        BundleStore store = new BundleStore(root());
        stageOk(store, "one", spa());
        store.onBoot(1);
        store.ready();

        BundleStore.State first = store.read();
        assertEquals("", first.pendingHealth);
        assertEquals("one", first.lastKnownGood);

        // A second bundle arrives, boots, and reports healthy. The one
        // before last is what gets reaped: the previous good bundle is
        // kept until the new one has proved itself.
        Map<String, byte[]> second = spa();
        second.put("assets/app.js", "export const a = 2;\n".getBytes(StandardCharsets.UTF_8));
        stageOk(store, "two", second);
        store.onBoot(1);
        assertTrue("the previous good bundle stays until this one proves itself",
                store.dir("one").isDirectory());
        store.ready();

        assertEquals("two", store.read().lastKnownGood);
        assertFalse("a bundle that is neither current nor last-known-good is reaped",
                store.dir("one").exists());
        assertTrue(store.dir("two").isDirectory());
    }

    @Test
    public void aBundleStagedBeforeTheHealthReportSurvivesIt() throws Exception {
        // The order on a fast link: the shell stages the download and
        // only then confirms this launch healthy. The reap that report
        // triggers must not take the bundle the person was just told
        // about.
        BundleStore store = new BundleStore(root());
        stageOk(store, "one", spa());
        store.onBoot(1);
        Map<String, byte[]> second = spa();
        second.put("assets/app.js", "export const a = 2;\n".getBytes(StandardCharsets.UTF_8));
        stageOk(store, "two", second);
        store.ready();

        BundleStore.State state = store.read();
        assertEquals("two", state.next);
        assertTrue("a staged bundle waits for the next cold start", store.dir("two").isDirectory());
        assertEquals("the next cold start adopts what the healthy report did not lose",
                "two", store.onBoot(1).getName());
        assertEquals("", store.read().next);
    }

    @Test
    public void aBootThatNeverReportsHealthyRollsBack() throws Exception {
        BundleStore store = new BundleStore(root());
        stageOk(store, "good", spa());
        store.onBoot(1);
        store.ready();

        Map<String, byte[]> broken = spa();
        broken.put("assets/app.js", "throw new Error('boom')\n".getBytes(StandardCharsets.UTF_8));
        stageOk(store, "bad", broken);
        assertEquals(store.dir("bad"), store.onBoot(1));

        // The launch dies without ever calling ready(). The NEXT launch
        // is what notices, because pendingHealth survived it.
        File serving = store.onBoot(1);
        assertEquals("the roll back lands on the last known good bundle",
                store.dir("good"), serving);

        BundleStore.State state = store.read();
        assertEquals("good", state.current);
        assertEquals("", state.pendingHealth);
        assertTrue("the failed id is remembered so it is not downloaded again",
                state.rolledBack.contains("bad"));
        assertFalse("a bundle that failed is deleted", store.dir("bad").exists());
    }

    @Test
    public void aRollbackWithNoKnownGoodLandsOnTheApkAssets() throws Exception {
        BundleStore store = new BundleStore(root());
        stageOk(store, "bad", spa());
        store.onBoot(1);

        assertNull("with nothing good behind it, the fallback is the APK", store.onBoot(1));
        BundleStore.State state = store.read();
        assertEquals("", state.current);
        assertTrue(state.rolledBack.contains("bad"));
    }

    @Test
    public void theWatchdogRollsBackInPlace() throws Exception {
        BundleStore store = new BundleStore(root());
        stageOk(store, "good", spa());
        store.onBoot(1);
        store.ready();

        Map<String, byte[]> hangs = spa();
        hangs.put("assets/app.js", "while (true) {}\n".getBytes(StandardCharsets.UTF_8));
        stageOk(store, "hangs", hangs);
        store.onBoot(1);
        assertTrue(store.awaitingHealth());

        assertEquals("the watchdog falls back without a restart",
                store.dir("good"), store.rollbackUnhealthy());
        assertFalse(store.awaitingHealth());
        assertTrue(store.read().rolledBack.contains("hangs"));
    }

    @Test
    public void theRolledBackListSurvivesAnOrdinaryBoot() throws Exception {
        BundleStore store = new BundleStore(root());
        stageOk(store, "good", spa());
        store.onBoot(1);
        store.ready();

        Map<String, byte[]> broken = spa();
        broken.put("index.html", "<!doctype html><title>broken</title>".getBytes(StandardCharsets.UTF_8));
        stageOk(store, "bad", broken);
        store.onBoot(1);
        store.onBoot(1);
        assertTrue(store.read().rolledBack.contains("bad"));

        // An ordinary relaunch on the good bundle must NOT forget it, or
        // the shell would download the same failure again.
        store.onBoot(1);
        store.ready();
        assertTrue("only a DIFFERENT id succeeding may clear the list",
                store.read().rolledBack.contains("bad"));

        // A genuinely new bundle proving itself does clear it.
        Map<String, byte[]> fresh = spa();
        fresh.put("assets/app.js", "export const a = 3;\n".getBytes(StandardCharsets.UTF_8));
        stageOk(store, "fresh", fresh);
        store.onBoot(1);
        store.ready();
        assertTrue("a new bundle that works clears the list",
                store.read().rolledBack.isEmpty());
    }

    @Test
    public void aBundleDirectoryThatVanishedFallsBackToTheAssets() throws Exception {
        BundleStore store = new BundleStore(root());
        stageOk(store, "abc123", spa());
        store.onBoot(1);
        store.ready();
        BundleStore.deleteRecursively(store.dir("abc123"));

        assertNull("a missing bundle must not become a white screen", store.onBoot(1));
        assertEquals("", store.read().current);
    }

    @Test
    public void aDamagedStateFileReadsAsTheApkAssets() throws Exception {
        BundleStore store = new BundleStore(root());
        stageOk(store, "abc123", spa());
        File stateFile = new File(root(), BundleStore.STATE_FILE);
        Files.write(stateFile.toPath(), "{not json".getBytes(StandardCharsets.UTF_8));

        BundleStore.State state = store.read();
        assertEquals("", state.current);
        assertEquals("", state.next);
        List<String> empty = new ArrayList<>();
        assertEquals(empty, state.rolledBack);
        assertNull(store.onBoot(1));
    }
}
