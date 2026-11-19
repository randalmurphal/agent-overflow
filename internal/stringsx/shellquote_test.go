package stringsx

import (
	"os/exec"
	"testing"
)

func TestShellQuoteIsOneWord(t *testing.T) {
	t.Parallel()
	if got, want := ShellQuote(`it's fine`), `'it'\''s fine'`; got != want {
		t.Fatalf("ShellQuote = %q, want %q", got, want)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, word := range []string{"", "plain", "it's", "''", `a "b" $c`, "`d` \\e\nf", "$(g) * ?"} {
		out, err := exec.Command(sh, "-c", "printf %s "+ShellQuote(word)).Output()
		if err != nil || string(out) != word {
			t.Errorf("sh read %q back as %q, %v", word, out, err)
		}
	}
}
