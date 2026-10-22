package dev.agentoverflow.app.speech;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNull;

import java.util.Arrays;
import java.util.Collections;
import java.util.List;

import org.junit.Test;

public class VoiceChoiceTest {

    private static final List<String> INSTALLED = Arrays.asList("en-gb-x-1", "en-us-x-2");

    @Test
    public void usesTheVoiceAskedFor() {
        assertEquals("en-us-x-2", VoiceChoice.choose(INSTALLED, "en-us-x-2"::equals, "en-gb-x-1"));
    }

    @Test
    public void fallsBackToTheEngineDefaultForAnEmptyOrRemovedVoice() {
        assertEquals("en-gb-x-1", VoiceChoice.choose(INSTALLED, ""::equals, "en-gb-x-1"));
        assertEquals("en-gb-x-1", VoiceChoice.choose(INSTALLED, "gone"::equals, "en-gb-x-1"));
    }

    @Test
    public void readsInAnInstalledVoiceWhenTheEngineHasNoDefault() {
        assertEquals("en-gb-x-1", VoiceChoice.choose(INSTALLED, ""::equals, null));
    }

    @Test
    public void choosesNothingWhenNoVoiceIsInstalled() {
        assertNull(VoiceChoice.choose(Collections.<String>emptyList(), ""::equals, null));
        assertNull(VoiceChoice.choose(null, ""::equals, null));
    }
}
