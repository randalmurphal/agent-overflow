package wsllauncher

import "strings"

// AppendWSLENV returns env with the named variables added to WSLENV so
// wsl.exe (and Windows-process interop in the other direction) carries
// them across the Windows/WSL boundary. Names already listed in an
// existing WSLENV entry are not duplicated; names not present in env at
// all are skipped — WSLENV passthrough for an unset variable is a
// no-op and listing it just adds noise.
//
// env has os.Environ() shape ("KEY=value"). The returned slice is a
// copy when a change is needed and the input unchanged otherwise.
func AppendWSLENV(env []string, names ...string) []string {
	present := func(key string) bool {
		for _, kv := range env {
			if len(kv) > len(key) && kv[len(key)] == '=' && strings.EqualFold(kv[:len(key)], key) {
				return true
			}
		}
		return false
	}

	existing := ""
	existingIdx := -1
	for i, kv := range env {
		if len(kv) > 7 && strings.EqualFold(kv[:7], "WSLENV=") {
			existing = kv[7:]
			existingIdx = i
		}
	}
	listed := map[string]bool{}
	for _, entry := range strings.Split(existing, ":") {
		// Entries can carry translation flags ("PATH/l"); the name is
		// the part before the first '/'.
		name, _, _ := strings.Cut(entry, "/")
		if name != "" {
			listed[name] = true
		}
	}

	added := existing
	for _, name := range names {
		if listed[name] || !present(name) {
			continue
		}
		if added == "" {
			added = name
		} else {
			added += ":" + name
		}
	}
	if added == existing {
		return env
	}

	out := make([]string, len(env))
	copy(out, env)
	if existingIdx >= 0 {
		out[existingIdx] = "WSLENV=" + added
		return out
	}
	return append(out, "WSLENV="+added)
}
