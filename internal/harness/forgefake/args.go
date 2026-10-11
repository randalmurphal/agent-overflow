package forgefake

import (
	"fmt"
	"strings"
)

// flagDef is one flag a command accepts. A command's flag list is
// closed: a flag the real CLI knows but no handler reads is an unknown
// flag here, so an app change that adds one fails loudly instead of
// being answered as if it had not been passed.
type flagDef struct {
	long  string
	short string
	value bool
}

// call is one parsed invocation. A REST request the HTTP mounts received
// arrives as the `api` call it stands for, with http set: its endpoint
// is the one positional, --method and --hostname are synthesized from
// the request, and handlers answer status and headers through the
// response instead of exit codes and --include text. A GraphQL request
// carries graphQL instead of an endpoint.
type call struct {
	cli     string
	args    []string
	cwd     string
	stdin   []byte
	http    *httpCall
	graphQL *graphQLRequest

	flags      map[string][]string
	positional []string
}

// httpCall is what an HTTP request carries beyond its `api` shape. host
// is the forge host the request is for (its Host header), spelled as
// PRReference.Host: a port is allowed and compared as given. base is the
// fake listener's own URL ("http://[::1]:port"), which a next-page link
// points at.
type httpCall struct {
	host string
	base string
}

func (c *call) flag(long string) string {
	values := c.flags[long]
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

func (c *call) has(long string) bool {
	_, ok := c.flags[long]
	return ok
}

// parseFlags splits args (the words after the command path) into flags
// and positionals under defs. `--name=value`, `--name value`, `-n value`
// and `-nvalue` are the spellings both CLIs accept for a value flag.
func parseFlags(args []string, defs []flagDef) (map[string][]string, []string, error) {
	flags := make(map[string][]string)
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		var def *flagDef
		inline, hasInline := "", false
		if name, ok := strings.CutPrefix(arg, "--"); ok {
			name, inline, hasInline = strings.Cut(name, "=")
			def = findFlag(defs, func(d flagDef) bool { return d.long == name })
		} else {
			short := arg[1:2]
			def = findFlag(defs, func(d flagDef) bool { return d.short == short })
			if len(arg) > 2 {
				inline, hasInline = arg[2:], true
			}
		}
		if def == nil {
			return nil, nil, fmt.Errorf("unknown flag %s", arg)
		}
		if !def.value {
			if hasInline {
				return nil, nil, fmt.Errorf("flag %s takes no value", arg)
			}
			flags[def.long] = append(flags[def.long], "")
			continue
		}
		if !hasInline {
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("flag %s needs a value", arg)
			}
			i++
			inline = args[i]
		}
		flags[def.long] = append(flags[def.long], inline)
	}
	return flags, positional, nil
}

func findFlag(defs []flagDef, match func(flagDef) bool) *flagDef {
	for i := range defs {
		if match(defs[i]) {
			return &defs[i]
		}
	}
	return nil
}
