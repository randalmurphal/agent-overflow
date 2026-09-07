package dev.agentoverflow.app;

import static org.junit.Assert.*;
import org.junit.Test;

public class ReleaseVersionTest {
    @Test
    public void semverPrecedenceHandlesPrereleasesWithoutNumericOverflow() {
        String[] ordered = {"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
                "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.10.0", "2.0.0",
                "999999999999999999999999999.0.0"};
        for (int i = 1; i < ordered.length; i++) {
            assertTrue(ordered[i], ReleaseVersion.parse(ordered[i]).compareTo(ReleaseVersion.parse(ordered[i-1])) > 0);
        }
        assertEquals(0, ReleaseVersion.parse("1.0.0+abc").compareTo(ReleaseVersion.parse("1.0.0+def")));
        assertEquals(0, ReleaseVersion.parse("1.0.0-rc.1+a").compareTo(ReleaseVersion.parse("1.0.0-rc.1+b")));
    }

    @Test
    public void malformedVersionsHaveNoOrdering() {
        for (String value : new String[]{"", "v1.0.0", "1.0", "01.0.0", "1.0.0-01", "1.0.0-", "1.0.0+",
                "1.0.0-a..b", " 1.0.0", "1.0.0\\n", "1.0.0+bad_", "1".repeat(257)}) {
            assertNull(value, ReleaseVersion.parse(value));
        }
    }
}
