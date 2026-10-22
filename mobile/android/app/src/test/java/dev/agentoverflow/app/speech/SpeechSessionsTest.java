package dev.agentoverflow.app.speech;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNull;

import org.junit.Test;

public class SpeechSessionsTest {

    @Test
    public void reportsTheCurrentReadingsPiecesAndItsEnd() {
        SpeechSessions sessions = new SpeechSessions();
        sessions.begin("s1", 2);
        SpeechSessions.Event started = sessions.started(SpeechSessions.utteranceId("s1", 1));
        assertEquals("progress", started.kind);
        assertEquals(1, started.index);
        assertNull("a piece before the last does not end the reading",
                sessions.finished(SpeechSessions.utteranceId("s1", 1)));
        SpeechSessions.Event ended = sessions.finished(SpeechSessions.utteranceId("s1", 2));
        assertEquals("done", ended.kind);
        assertEquals("ended", ended.outcome);
        assertNull("nothing follows the end", sessions.started(SpeechSessions.utteranceId("s1", 2)));
    }

    @Test
    public void dropsWhatAReplacedReadingReports() {
        SpeechSessions sessions = new SpeechSessions();
        sessions.begin("s1", 3);
        sessions.begin("s2", 1);
        assertNull(sessions.started(SpeechSessions.utteranceId("s1", 0)));
        assertNull(sessions.finished(SpeechSessions.utteranceId("s1", 3)));
        assertNull(sessions.failed(SpeechSessions.utteranceId("s1", 0), "x"));
        assertNull("a replaced reading cannot be stopped", sessions.stop("s1"));
        assertEquals("progress", sessions.started(SpeechSessions.utteranceId("s2", 0)).kind);
    }

    @Test
    public void stopsOnlyTheCurrentReadingAndOnce() {
        SpeechSessions sessions = new SpeechSessions();
        sessions.begin("s1", 1);
        assertEquals("stopped", sessions.stop("s1").outcome);
        assertNull(sessions.stop("s1"));
        assertNull("a stopped reading's interrupted piece is not a failure",
                sessions.failed(SpeechSessions.utteranceId("s1", 0), "interrupted"));
    }

    @Test
    public void reportsAFailureOnceWithItsMessage() {
        SpeechSessions sessions = new SpeechSessions();
        sessions.begin("s1", 1);
        SpeechSessions.Event failed = sessions.failed(SpeechSessions.utteranceId("s1", 0), "the speech engine failed (-4)");
        assertEquals("error", failed.outcome);
        assertEquals("the speech engine failed (-4)", failed.message);
        assertNull(sessions.finished(SpeechSessions.utteranceId("s1", 1)));
    }

    @Test
    public void ignoresMalformedUtteranceIds() {
        SpeechSessions sessions = new SpeechSessions();
        sessions.begin("s1", 0);
        assertNull(sessions.started(null));
        assertNull(sessions.started("s1"));
        assertNull(sessions.started("s1:x"));
        assertNull(sessions.started(":0"));
    }
}
