package dev.agentoverflow.app.speech;

import java.util.function.Predicate;

/**
 * Which installed voice a reading uses: the one asked for, else the
 * engine's default if it is installed, else the first installed one. The
 * engine's default can be null, or a voice whose data is not on the phone,
 * while other voices are installed; an empty request or a removed voice
 * must still read. No Android type, so it is covered by a JVM test.
 */
final class VoiceChoice {

    private VoiceChoice() {
    }

    /** Null only when no voice is installed at all. */
    static <V> V choose(Iterable<V> installed, Predicate<V> requested, V engineDefault) {
        V first = null;
        boolean defaultInstalled = false;
        if (installed != null) {
            for (V voice : installed) {
                if (requested.test(voice)) {
                    return voice;
                }
                if (first == null) {
                    first = voice;
                }
                if (voice.equals(engineDefault)) {
                    defaultInstalled = true;
                }
            }
        }
        return defaultInstalled ? engineDefault : first;
    }
}
