package dev.agentoverflow.app.speech;

/**
 * Which reading is current and what TextToSpeech's callbacks mean for it.
 *
 * <p>One reading is a session: its pieces are queued as utterances whose
 * ids carry the session and the piece index. Callbacks for any session but
 * the current one belong to a reading that was stopped or replaced and are
 * dropped. No Android type, so the bookkeeping is covered by a JVM test.
 */
final class SpeechSessions {

    /** What a callback means for the page. */
    static final class Event {
        final String kind;
        final String session;
        final int index;
        final String outcome;
        final String message;

        private Event(String kind, String session, int index, String outcome, String message) {
            this.kind = kind;
            this.session = session;
            this.index = index;
            this.outcome = outcome;
            this.message = message;
        }

        static Event progress(String session, int index) {
            return new Event("progress", session, index, null, null);
        }

        static Event done(String session, String outcome, String message) {
            return new Event("done", session, -1, outcome, message);
        }
    }

    private String current;
    private int last = -1;

    /** Make {@code session} current, reading through piece {@code last}. */
    synchronized void begin(String session, int last) {
        this.current = session;
        this.last = last;
    }

    static String utteranceId(String session, int index) {
        return session + ":" + index;
    }

    /**
     * Ends the current session if it is {@code session}. Returns the event
     * to send, or null when that session is not the one playing.
     */
    synchronized Event stop(String session) {
        if (current == null || !current.equals(session)) {
            return null;
        }
        current = null;
        return Event.done(session, "stopped", null);
    }

    synchronized Event started(String utteranceId) {
        Piece piece = Piece.parse(utteranceId);
        if (piece == null || !piece.session.equals(current)) {
            return null;
        }
        return Event.progress(piece.session, piece.index);
    }

    synchronized Event finished(String utteranceId) {
        Piece piece = Piece.parse(utteranceId);
        if (piece == null || !piece.session.equals(current) || piece.index != last) {
            return null;
        }
        current = null;
        return Event.done(piece.session, "ended", null);
    }

    synchronized Event failed(String utteranceId, String message) {
        Piece piece = Piece.parse(utteranceId);
        if (piece == null || !piece.session.equals(current)) {
            return null;
        }
        current = null;
        return Event.done(piece.session, "error", message);
    }

    private static final class Piece {
        final String session;
        final int index;

        private Piece(String session, int index) {
            this.session = session;
            this.index = index;
        }

        static Piece parse(String utteranceId) {
            if (utteranceId == null) {
                return null;
            }
            int colon = utteranceId.lastIndexOf(':');
            if (colon <= 0) {
                return null;
            }
            try {
                return new Piece(utteranceId.substring(0, colon), Integer.parseInt(utteranceId.substring(colon + 1)));
            } catch (NumberFormatException malformed) {
                return null;
            }
        }
    }
}
