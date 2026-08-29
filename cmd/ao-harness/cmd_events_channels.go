package main

func eventsChannels(e *env, args []string) error {
	flags := e.newFlagSet("events channels [pattern]")
	asJSON := e.bindJSONFlag(flags)
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if *asJSON {
		e.format = "json"
	}
	if len(rest) > 1 {
		return usagef("events channels takes at most one pattern (got %v)", rest)
	}
	names := knownChannels()
	if len(rest) == 1 {
		names = filterContains(names, rest[0])
	}
	if e.jsonOutput() {
		return e.writeJSON(names)
	}
	if len(names) == 0 {
		pattern := ""
		if len(rest) == 1 {
			pattern = rest[0]
		}
		e.printf("no channel matches %q\n", pattern)
		return nil
	}
	for _, name := range names {
		e.printf("%s\n", name)
	}
	return nil
}
