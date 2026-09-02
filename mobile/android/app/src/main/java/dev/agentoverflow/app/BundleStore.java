package dev.agentoverflow.app;

import java.io.ByteArrayInputStream;
import java.io.File;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.HashSet;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Set;
import java.util.zip.ZipEntry;
import java.util.zip.ZipInputStream;

import org.json.JSONArray;
import org.json.JSONException;
import org.json.JSONObject;

/**
 * Which web bundle this shell runs, and everything that decides it.
 *
 * <p>Deliberately a PLAIN class with no Android type in its signature: it
 * takes a directory and does file work, so the whole of the update
 * mechanic — the state transitions, the unzip, the verification, the
 * rollback — is exercised by an ordinary JVM unit test rather than by an
 * emulator run nobody can perform on the development box. {@link
 * BundlePlugin} is the Capacitor wrapper around it and {@code
 * MainActivity} is its boot caller.
 *
 * <p><b>The layout.</b> {@code filesDir/bundles/} holds one directory per
 * bundle, named by its content id, plus {@code state.json}:
 *
 * <pre>{current, next, pendingHealth, lastKnownGood, rolledBack: []}</pre>
 *
 * <p>Each of the four strings is a bundle id or {@code ""}. <b>An empty
 * {@code current} means the APK's own assets</b> — the bundle this build
 * shipped with — and it is the resting state of a phone that has never
 * updated, the state a rollback returns to when there is no last known
 * good, and the state a damaged state file reads as. There is no separate
 * "using assets" flag, because a second spelling of the same fact is a
 * second thing to keep true.
 *
 * <p><b>All state access is serialized.</b> Capacitor runs plugin methods
 * on its own task thread while the boot transition and the health
 * watchdog run on the main one, so every read-modify-write below holds
 * one process-wide lock. The file is small and the operations are rare;
 * the lock costs nothing and removes a class of interleaving nobody would
 * reproduce.
 */
final class BundleStore {

    /** The state file, beside the bundle directories it describes. */
    static final String STATE_FILE = "state.json";

    /** Suffix a bundle wears while it is being written and verified. */
    static final String STAGING_SUFFIX = ".staging";

    /**
     * The file whose presence makes a bundle directory servable. A
     * directory without it is a bundle that was interrupted between the
     * rename and the next boot, and serving it would be a white screen.
     */
    static final String ENTRY_FILE = "index.html";

    /**
     * One process-wide lock over the state file. Static because two
     * BundleStore instances over one directory are the normal case —
     * MainActivity builds one for boot and the plugin builds its own —
     * and they must not be two independent writers.
     */
    private static final Object LOCK = new Object();

    private final File root;

    BundleStore(File root) {
        this.root = root;
    }

    /** The mutable half of the state file, as one value. */
    static final class State {
        String current = "";
        String next = "";
        String pendingHealth = "";
        String lastKnownGood = "";
        final List<String> rolledBack = new ArrayList<>();

        JSONObject toJson() throws JSONException {
            JSONObject json = new JSONObject();
            json.put("current", current);
            json.put("next", next);
            json.put("pendingHealth", pendingHealth);
            json.put("lastKnownGood", lastKnownGood);
            json.put("rolledBack", new JSONArray(rolledBack));
            return json;
        }
    }

    /** One file as the manifest describes it. */
    static final class FileSpec {
        final String sha256;
        final long size;

        FileSpec(String sha256, long size) {
            this.sha256 = sha256;
            this.size = size;
        }
    }

    /** Raised by {@link #stage} with a message naming what was wrong. */
    static final class BundleException extends Exception {
        BundleException(String message) {
            super(message);
        }

        BundleException(String message, Throwable cause) {
            super(message, cause);
        }
    }

    // -----------------------------------------------------------------
    // State
    // -----------------------------------------------------------------

    /**
     * Read the state file.
     *
     * <p>A missing, unreadable or malformed file reads as the empty state
     * — a phone running the APK's own assets — rather than as an error.
     * That is the honest recovery: the directories on disk are still
     * verified content, and the worst outcome of forgetting them is one
     * download. Refusing to boot over a damaged 200-byte file would be
     * worse than anything it could have said.
     */
    State read() {
        synchronized (LOCK) {
            return readLocked();
        }
    }

    private State readLocked() {
        State state = new State();
        File file = new File(root, STATE_FILE);
        if (!file.isFile()) {
            return state;
        }
        try {
            JSONObject json = new JSONObject(new String(readAll(file), StandardCharsets.UTF_8));
            state.current = json.optString("current", "");
            state.next = json.optString("next", "");
            state.pendingHealth = json.optString("pendingHealth", "");
            state.lastKnownGood = json.optString("lastKnownGood", "");
            JSONArray rolled = json.optJSONArray("rolledBack");
            if (rolled != null) {
                for (int i = 0; i < rolled.length(); i++) {
                    String id = rolled.optString(i, "");
                    if (!id.isEmpty()) {
                        state.rolledBack.add(id);
                    }
                }
            }
        } catch (JSONException | IOException damaged) {
            return new State();
        }
        return state;
    }

    /**
     * Write the state file, whole, through a temporary name.
     *
     * <p>Rename rather than truncate-and-write: this file is read on the
     * next cold start, and a process killed halfway through a rewrite
     * would leave the one document that decides which bundle boots
     * half-written.
     */
    private void writeLocked(State state) throws IOException {
        if (!root.isDirectory() && !root.mkdirs()) {
            throw new IOException("cannot create " + root);
        }
        File temp = new File(root, STATE_FILE + ".tmp");
        try {
            byte[] encoded = state.toJson().toString().getBytes(StandardCharsets.UTF_8);
            try (FileOutputStream out = new FileOutputStream(temp)) {
                out.write(encoded);
                out.getFD().sync();
            }
        } catch (JSONException impossible) {
            throw new IOException("encode bundle state", impossible);
        }
        File target = new File(root, STATE_FILE);
        if (!temp.renameTo(target)) {
            // renameTo will not replace on some filesystems. Deleting
            // first is the fallback and is safe: the temp file already
            // holds the whole document.
            deleteRecursively(target);
            if (!temp.renameTo(target)) {
                throw new IOException("cannot install " + target);
            }
        }
    }

    /** The directory one bundle id lives in. */
    File dir(String id) {
        return new File(root, id);
    }

    // -----------------------------------------------------------------
    // Boot
    // -----------------------------------------------------------------

    /**
     * Apply this launch's transition and answer the directory to serve,
     * or {@code null} for the APK's own assets.
     *
     * <p>Three cases, in this order, and the order is the whole design:
     *
     * <ol>
     *   <li><b>{@code pendingHealth} is still set.</b> The previous launch
     *       swapped onto a bundle and never reported healthy — it hung,
     *       it crashed, or the person killed it. Roll back: {@code
     *       current} becomes {@code lastKnownGood}, the bad id joins
     *       {@code rolledBack}, its directory is deleted. Recording the
     *       id is what stops the shell downloading the same broken
     *       bundle again on the next hello.
     *   <li><b>{@code next} is set.</b> A verified bundle is waiting.
     *       Adopt it and arm the health check by setting {@code
     *       pendingHealth} to it: from here until the app reports
     *       healthy, case 1 is what happens on any launch.
     *   <li>Otherwise nothing moves.
     * </ol>
     *
     * <p>Then the directory is checked for real: a {@code current} whose
     * directory is missing or has no {@code index.html} falls back to the
     * assets and is cleared, so a bundle lost to a wipe or an interrupted
     * install cannot become a white screen.
     */
    File onBoot() {
        synchronized (LOCK) {
            State state = readLocked();
            if (!state.pendingHealth.isEmpty()) {
                rollbackLocked(state);
            } else if (!state.next.isEmpty()) {
                state.current = state.next;
                state.next = "";
                state.pendingHealth = state.current;
            }
            File serving = resolveLocked(state);
            try {
                writeLocked(state);
            } catch (IOException ignored) {
                // A state file we could not write means the next launch
                // repeats this decision. That is survivable and is not a
                // reason to refuse to show the app.
            }
            return serving;
        }
    }

    /**
     * The rollback, shared by the boot path and the health watchdog.
     * Mutates {@code state}; the caller writes it.
     */
    private void rollbackLocked(State state) {
        String bad = state.pendingHealth;
        state.pendingHealth = "";
        if (!bad.isEmpty() && !state.rolledBack.contains(bad)) {
            state.rolledBack.add(bad);
        }
        state.current = state.lastKnownGood;
        state.next = "";
        if (!bad.isEmpty() && !bad.equals(state.lastKnownGood)) {
            deleteRecursively(dir(bad));
        }
    }

    /**
     * Resolve {@code current} to a servable directory, clearing it when
     * there is nothing there. Mutates {@code state}; the caller writes.
     */
    private File resolveLocked(State state) {
        if (state.current.isEmpty()) {
            return null;
        }
        File dir = dir(state.current);
        if (dir.isDirectory() && new File(dir, ENTRY_FILE).isFile()) {
            return dir;
        }
        state.current = "";
        state.pendingHealth = "";
        return null;
    }

    /**
     * Roll back from a launch that never reported healthy, and answer the
     * directory to fall back onto ({@code null} for the assets).
     *
     * <p>The watchdog's entry point. Answers {@code null} through {@code
     * rolledBackTo} only when there is something to do — a caller checks
     * {@link #awaitingHealth()} first, because "no fallback directory"
     * and "nothing to roll back" are different answers that happen to
     * share a value.
     */
    File rollbackUnhealthy() {
        synchronized (LOCK) {
            State state = readLocked();
            if (state.pendingHealth.isEmpty()) {
                return resolveLocked(state);
            }
            rollbackLocked(state);
            File serving = resolveLocked(state);
            try {
                writeLocked(state);
            } catch (IOException ignored) {
                // Same reasoning as onBoot: the next launch repeats it.
            }
            return serving;
        }
    }

    /** Whether this launch is still waiting to be told it is healthy. */
    boolean awaitingHealth() {
        return !read().pendingHealth.isEmpty();
    }

    /**
     * The app booted and is running. Clear the health check, promote the
     * running bundle to last-known-good, and reap every directory the
     * state no longer names.
     *
     * <p>{@code rolledBack} is cleared only when THIS launch was the
     * first on a new bundle — which is exactly the case {@code
     * pendingHealth} was set. A launch that merely booted the existing
     * bundle again must keep the list, or the shell would re-download a
     * bundle it has already watched fail.
     */
    void ready() {
        synchronized (LOCK) {
            State state = readLocked();
            boolean firstBootOnANewBundle = !state.pendingHealth.isEmpty();
            state.pendingHealth = "";
            state.lastKnownGood = state.current;
            if (firstBootOnANewBundle) {
                state.rolledBack.clear();
            }
            pruneLocked(state);
            try {
                writeLocked(state);
            } catch (IOException ignored) {
                // The next launch would re-arm a health check for a
                // bundle that is working. That is a wasted rollback at
                // worst, not a broken app.
            }
        }
    }

    /**
     * Delete every bundle directory the state file no longer names.
     *
     * <p>{@code next} is kept too: a bundle staged before this launch
     * reported healthy is waiting for the next cold start, and reaping
     * it here would be a download the person was told about that then
     * never loads.
     */
    private void pruneLocked(State state) {
        File[] entries = root.listFiles();
        if (entries == null) {
            return;
        }
        Set<String> keep = new HashSet<>();
        if (!state.current.isEmpty()) {
            keep.add(state.current);
        }
        if (!state.next.isEmpty()) {
            keep.add(state.next);
        }
        if (!state.lastKnownGood.isEmpty()) {
            keep.add(state.lastKnownGood);
        }
        for (File entry : entries) {
            if (!entry.isDirectory() || keep.contains(entry.getName())) {
                continue;
            }
            deleteRecursively(entry);
        }
    }

    // -----------------------------------------------------------------
    // Staging
    // -----------------------------------------------------------------

    /**
     * Unzip and verify one downloaded bundle, then make it {@code next}.
     *
     * <p>Verification happens WHILE the archive is being written, entry
     * by entry, and every one of these is a refusal:
     *
     * <ul>
     *   <li>an entry the manifest does not name;
     *   <li>a path that is absolute, contains {@code ..}, or otherwise
     *       resolves outside the staging directory;
     *   <li>a directory entry, which this archive never contains;
     *   <li>a file whose size or SHA-256 disagrees with the manifest;
     *   <li>a manifest path the archive never delivered.
     * </ul>
     *
     * <p>Nothing partially verified is ever visible: the work happens
     * under {@code <id>.staging} and the rename to {@code <id>} is the
     * last step. A failure deletes the staging directory and throws with
     * a message naming the file, because "the update failed" is not
     * something anybody can act on.
     */
    void stage(String id, Map<String, FileSpec> manifest, byte[] archive) throws BundleException {
        if (id == null || id.isEmpty()) {
            throw new BundleException("a bundle needs an id");
        }
        if (manifest.isEmpty()) {
            throw new BundleException("bundle " + id + " has an empty manifest");
        }
        File staging = new File(root, id + STAGING_SUFFIX);
        deleteRecursively(staging);
        if (!staging.mkdirs()) {
            throw new BundleException("cannot create " + staging);
        }
        try {
            Set<String> delivered = unpack(staging, manifest, archive);
            for (String wanted : manifest.keySet()) {
                if (!delivered.contains(wanted)) {
                    throw new BundleException("the archive did not carry " + wanted);
                }
            }
            File target = dir(id);
            deleteRecursively(target);
            if (!staging.renameTo(target)) {
                throw new BundleException("cannot install bundle " + id);
            }
        } catch (BundleException | RuntimeException failure) {
            deleteRecursively(staging);
            throw failure;
        }
        synchronized (LOCK) {
            State state = readLocked();
            state.next = id;
            try {
                writeLocked(state);
            } catch (IOException io) {
                deleteRecursively(dir(id));
                throw new BundleException("cannot record bundle " + id, io);
            }
        }
    }

    /** Write every entry, verifying as it goes. Answers what was delivered. */
    private Set<String> unpack(File staging, Map<String, FileSpec> manifest, byte[] archive)
            throws BundleException {
        Set<String> delivered = new LinkedHashSet<>();
        String stagingPath = canonical(staging);
        try (ZipInputStream zip = new ZipInputStream(new ByteArrayInputStream(archive))) {
            ZipEntry entry;
            while ((entry = zip.getNextEntry()) != null) {
                String name = entry.getName();
                if (entry.isDirectory()) {
                    throw new BundleException("the archive carries a directory entry: " + name);
                }
                FileSpec spec = manifest.get(name);
                if (spec == null) {
                    throw new BundleException("the manifest does not name " + name);
                }
                if (!delivered.add(name)) {
                    throw new BundleException("the archive carries " + name + " twice");
                }
                File out = new File(staging, name);
                // Checked against the resolved path rather than by looking
                // for "..": a canonical path that is not under the staging
                // directory is the whole question, however it was spelled.
                if (!canonical(out).startsWith(stagingPath + File.separator)) {
                    throw new BundleException("the archive names a path outside the bundle: " + name);
                }
                File parent = out.getParentFile();
                if (parent != null && !parent.isDirectory() && !parent.mkdirs()) {
                    throw new BundleException("cannot create a directory for " + name);
                }
                writeVerified(out, zip, name, spec);
            }
        } catch (IOException io) {
            throw new BundleException("cannot read the archive: " + io.getMessage(), io);
        }
        return delivered;
    }

    /** Stream one entry to disk, hashing it, and refuse a disagreement. */
    private void writeVerified(File out, ZipInputStream zip, String name, FileSpec spec)
            throws BundleException, IOException {
        MessageDigest digest;
        try {
            digest = MessageDigest.getInstance("SHA-256");
        } catch (NoSuchAlgorithmException impossible) {
            throw new BundleException("this device has no SHA-256", impossible);
        }
        long written = 0;
        byte[] buffer = new byte[32 * 1024];
        try (OutputStream sink = new FileOutputStream(out)) {
            int read;
            while ((read = zip.read(buffer)) > 0) {
                written += read;
                if (written > spec.size) {
                    throw new BundleException(name + " is longer than the manifest says");
                }
                digest.update(buffer, 0, read);
                sink.write(buffer, 0, read);
            }
        }
        if (written != spec.size) {
            throw new BundleException(name + " is " + written + " bytes, the manifest says " + spec.size);
        }
        String actual = hex(digest.digest());
        if (!actual.equalsIgnoreCase(spec.sha256)) {
            throw new BundleException(name + " does not match its digest");
        }
    }

    // -----------------------------------------------------------------
    // Manifest
    // -----------------------------------------------------------------

    /**
     * Read the manifest's file list into the shape staging needs.
     *
     * <p>Every entry is validated here rather than trusted: a path that
     * could not be written safely is refused where the document is read,
     * not discovered halfway through an unzip. The rule mirrors
     * {@code internal/bundle.CleanPath} on the producing side, which is
     * the arrangement intended — the producer states it, the consumer
     * enforces it, and neither has to trust the other.
     */
    static Map<String, FileSpec> readManifest(JSONObject manifest) throws BundleException {
        JSONArray files = manifest.optJSONArray("files");
        if (files == null || files.length() == 0) {
            throw new BundleException("the manifest names no files");
        }
        Map<String, FileSpec> specs = new HashMap<>(files.length() * 2);
        for (int i = 0; i < files.length(); i++) {
            JSONObject file = files.optJSONObject(i);
            if (file == null) {
                throw new BundleException("the manifest has a malformed entry at " + i);
            }
            String path = file.optString("path", "");
            String sha256 = file.optString("sha256", "");
            long size = file.optLong("size", -1);
            if (!safePath(path)) {
                throw new BundleException("the manifest names an unusable path: " + path);
            }
            if (sha256.length() != 64 || size < 0) {
                throw new BundleException("the manifest describes " + path + " incompletely");
            }
            if (specs.put(path, new FileSpec(sha256, size)) != null) {
                throw new BundleException("the manifest names " + path + " twice");
            }
        }
        return specs;
    }

    /**
     * Whether a manifest path is one this shell may write under a bundle
     * directory: relative, slash-separated, no {@code .} or {@code ..}
     * element, no backslash, no drive letter, no empty segment.
     */
    static boolean safePath(String path) {
        if (path == null || path.isEmpty() || path.startsWith("/")) {
            return false;
        }
        if (path.indexOf('\\') >= 0 || path.indexOf(':') >= 0 || path.indexOf('\0') >= 0) {
            return false;
        }
        for (String element : path.split("/", -1)) {
            if (element.isEmpty() || element.equals(".") || element.equals("..")) {
                return false;
            }
        }
        return true;
    }

    // -----------------------------------------------------------------
    // Small helpers
    // -----------------------------------------------------------------

    private static String canonical(File file) throws BundleException {
        try {
            return file.getCanonicalPath();
        } catch (IOException io) {
            throw new BundleException("cannot resolve " + file, io);
        }
    }

    private static byte[] readAll(File file) throws IOException {
        return Files.readAllBytes(file.toPath());
    }

    private static String hex(byte[] bytes) {
        StringBuilder out = new StringBuilder(bytes.length * 2);
        for (byte b : bytes) {
            out.append(String.format(Locale.ROOT, "%02x", b));
        }
        return out.toString();
    }

    /** Delete a file or a whole directory, best effort. */
    static void deleteRecursively(File file) {
        if (file == null || !file.exists()) {
            return;
        }
        File[] children = file.listFiles();
        if (children != null) {
            for (File child : children) {
                deleteRecursively(child);
            }
        }
        // The return is deliberately ignored: a file we cannot delete is
        // reported by the next operation that needs it gone, and there is
        // nothing useful to do here.
        file.delete();
    }
}
