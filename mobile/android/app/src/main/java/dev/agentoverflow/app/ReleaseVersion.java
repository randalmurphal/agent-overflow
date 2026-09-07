package dev.agentoverflow.app;

import java.util.regex.Matcher;
import java.util.regex.Pattern;

/** Strict SemVer precedence; build metadata identifies no newer release. */
final class ReleaseVersion implements Comparable<ReleaseVersion> {
    private static final Pattern FORMAT = Pattern.compile(
            "(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)" +
            "(?:-([0-9A-Za-z-]+(?:\\.[0-9A-Za-z-]+)*))?" +
            "(?:\\+([0-9A-Za-z-]+(?:\\.[0-9A-Za-z-]+)*))?");
    private final String[] core, prerelease;

    private ReleaseVersion(String[] core, String[] prerelease) {
        this.core = core;
        this.prerelease = prerelease;
    }

    static ReleaseVersion parse(String text) {
        if (text == null || text.length() > 256) return null;
        Matcher match = FORMAT.matcher(text);
        if (!match.matches()) return null;
        String[] pre = match.group(4) == null ? new String[0] : match.group(4).split("\\.");
        for (String part : pre) {
            if (numeric(part) && part.length() > 1 && part.charAt(0) == '0') return null;
        }
        return new ReleaseVersion(new String[]{match.group(1), match.group(2), match.group(3)}, pre);
    }

    private static boolean numeric(String text) {
        for (int i = 0; i < text.length(); i++) {
            if (text.charAt(i) < '0' || text.charAt(i) > '9') return false;
        }
        return true;
    }

    private static int number(String a, String b) {
        return a.length() == b.length() ? a.compareTo(b) : Integer.compare(a.length(), b.length());
    }

    @Override
    public int compareTo(ReleaseVersion other) {
        for (int i = 0; i < core.length; i++) {
            int compared = number(core[i], other.core[i]);
            if (compared != 0) return compared;
        }
        if (prerelease.length == 0 || other.prerelease.length == 0) {
            return prerelease.length == other.prerelease.length ? 0 : prerelease.length == 0 ? 1 : -1;
        }
        for (int i = 0; i < Math.min(prerelease.length, other.prerelease.length); i++) {
            String a = prerelease[i], b = other.prerelease[i];
            boolean an = numeric(a), bn = numeric(b);
            int compared = an && bn ? number(a, b) : an != bn ? (an ? -1 : 1) : a.compareTo(b);
            if (compared != 0) return compared;
        }
        return Integer.compare(prerelease.length, other.prerelease.length);
    }
}
