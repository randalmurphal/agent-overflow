package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/harnessclient"
)

func runRPC(e *env, args []string) error {
	flags := e.newFlagSet("rpc <Method> [json args...]")
	list := flags.Bool("list", false, "print the method names this instance exposes (optionally filtered by a substring)")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if *list {
		pattern := ""
		if len(rest) > 0 {
			pattern = rest[0]
		}
		if len(rest) > 1 {
			return usagef("rpc --list takes at most one pattern (got %v)", rest)
		}
		return rpcList(e, pattern)
	}
	if len(rest) == 0 {
		return usagef("rpc needs a method name, e.g. `rpc HarnessListMocks` (or `rpc --list` for what exists)")
	}
	method := rest[0]
	params := make([]json.RawMessage, 0, len(rest)-1)
	for i, arg := range rest[1:] {
		if !json.Valid([]byte(arg)) {
			// A bare word is the mistake everyone makes first. Say what a
			// JSON value looks like rather than letting the server answer
			// bad_params.
			return usagef("argument %d (%q) is not a JSON value; strings need quotes, e.g. '\"thread-id\"'", i+1, arg)
		}
		params = append(params, json.RawMessage(arg))
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if err := requireHarnessProtocol(client, capabilityRequirements{Methods: []string{method}}); err != nil {
			return err
		}
		result, err := client.CallRaw(ctx, method, params)
		if err != nil {
			return suggestMethod(ctx, client, method, err)
		}
		return e.writeRawJSON(result)
	})
}

// rpcList prints what this instance will answer. The backend owns the
// list (HarnessListMethods), because a hand-kept copy here would be a
// second, wrong wire surface the moment a method is added.
func rpcList(e *env, pattern string) error {
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		names, err := listRPCMethods(ctx, client)
		if err != nil {
			return err
		}
		if pattern != "" {
			names = filterContains(names, pattern)
		}
		if e.jsonOutput() {
			return e.writeJSON(names)
		}
		if len(names) == 0 {
			e.printf("no method matches %q\n", pattern)
			return nil
		}
		for _, name := range names {
			e.printf("%s\n", name)
		}
		return nil
	})
}

func listRPCMethods(ctx context.Context, client *harnessclient.Client) ([]string, error) {
	raw, err := client.Call(ctx, "HarnessListMethods")
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil, fmt.Errorf("decode method list: %w", err)
	}
	sort.Strings(names)
	return names, nil
}

// suggestMethod turns "method_not_found" into the two or three names the
// caller probably meant. A typo in a method name is the most common way
// this command fails and the least self-explaining: the server's answer
// names the miss and nothing else.
func suggestMethod(ctx context.Context, client *harnessclient.Client, method string, callErr error) error {
	if !strings.Contains(callErr.Error(), "method_not_found") && !strings.Contains(callErr.Error(), "unknown method") {
		return callErr
	}
	names, err := listRPCMethods(ctx, client)
	if err != nil {
		// The instance cannot enumerate itself (an older backend). The
		// original refusal is still the answer.
		return callErr
	}
	near := nearestMethods(method, names)
	if len(near) == 0 {
		return fmt.Errorf("%w\n  `ao-harness rpc --list` prints all %d methods", callErr, len(names))
	}
	return fmt.Errorf("%w\n  did you mean: %s", callErr, strings.Join(near, ", "))
}

// nearestMethods ranks candidates by the cheapest signal that works on
// real typos: a case-insensitive substring either way (HarnessSeed vs
// seed), then a small edit distance. It is deliberately not a fuzzy
// matcher — the list is short and the answer only has to be a hint.
func nearestMethods(method string, names []string) []string {
	const maxSuggestions = 5
	lower := strings.ToLower(method)
	type scored struct {
		name string
		rank int
	}
	var out []scored
	for _, name := range names {
		candidate := strings.ToLower(name)
		switch {
		case strings.Contains(candidate, lower) || strings.Contains(lower, candidate):
			out = append(out, scored{name, 0})
		default:
			if d := editDistance(lower, candidate); d <= 3 {
				out = append(out, scored{name, d})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].rank != out[j].rank {
			return out[i].rank < out[j].rank
		}
		return out[i].name < out[j].name
	})
	names = names[:0:0]
	for _, s := range out {
		if len(names) == maxSuggestions {
			break
		}
		names = append(names, s.name)
	}
	return names
}

// editDistance is Levenshtein over two rows, which is all a suggestion
// needs. Bounded by the caller to short strings.
func editDistance(a, b string) int {
	if len(a) > len(b) {
		a, b = b, a
	}
	prev := make([]int, len(a)+1)
	curr := make([]int, len(a)+1)
	for i := range prev {
		prev[i] = i
	}
	for j := 1; j <= len(b); j++ {
		curr[0] = j
		for i := 1; i <= len(a); i++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[i] = min(prev[i]+1, min(curr[i-1]+1, prev[i-1]+cost))
		}
		prev, curr = curr, prev
	}
	return prev[len(a)]
}

func filterContains(values []string, pattern string) []string {
	lower := strings.ToLower(pattern)
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), lower) {
			out = append(out, value)
		}
	}
	return out
}

func runSeed(e *env, args []string) error {
	flags := e.newFlagSet("seed [-f <spec.json|->]")
	file := flags.String("f", "", "seed spec JSON file, or - for stdin")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return usagef("seed takes at most one spec file (got %v)", rest)
	}
	source := *file
	if source == "" && len(rest) == 1 {
		source = rest[0]
	}
	if source == "" {
		return usagef("seed needs a spec: `seed -f spec.json` or `seed -f -` to read stdin")
	}
	// Inline JSON on the command line is the mistake this command invites,
	// and `open <that>: file name too long` is a terrible way to learn it.
	if strings.HasPrefix(strings.TrimSpace(source), "{") {
		return usagef("that looks like inline JSON — pipe it: echo '...' | ao-harness seed -f -")
	}
	spec, err := readJSONDocument(source)
	if err != nil {
		return err
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if err := requireHarnessProtocol(client, capabilityRequirements{Methods: []string{"HarnessSeed"}}); err != nil {
			return err
		}
		result, err := client.CallRaw(ctx, "HarnessSeed", []json.RawMessage{spec})
		if err != nil {
			return err
		}
		if e.jsonOutput() {
			return e.writeRawJSON(result)
		}
		return e.printSeedSummary(result)
	})
}

// printSeedSummary is the -o text form: what a seed CREATED, not the
// document it created it with. -o json is still the server's own bytes.
func (e *env) printSeedSummary(raw json.RawMessage) error {
	var decoded struct {
		Projects []struct {
			ProjectID string   `json:"projectId"`
			Path      string   `json:"path"`
			ThreadIDs []string `json:"threadIds"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return e.writeRawJSON(raw)
	}
	threads := 0
	for _, project := range decoded.Projects {
		threads += len(project.ThreadIDs)
	}
	e.printf("seeded %d project(s), %d thread(s)\n", len(decoded.Projects), threads)
	for _, project := range decoded.Projects {
		e.printf("  %s  %d thread(s)  %s\n", project.ProjectID, len(project.ThreadIDs), project.Path)
	}
	return nil
}

func runReset(e *env, args []string) error {
	flags := e.newFlagSet("reset")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("reset takes no positional arguments (got %v)", rest)
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if err := requireHarnessProtocol(client, capabilityRequirements{Methods: []string{"HarnessReset"}}); err != nil {
			return err
		}
		if _, err := client.Call(ctx, "HarnessReset"); err != nil {
			return err
		}
		attached := pageIsAttached(ctx, e, client)
		if e.jsonOutput() {
			return e.writeJSON(map[string]any{"reset": true, "pageAttached": attached})
		}
		e.printf("reset\n")
		if attached {
			// NOT an auto-reload: HarnessReset deletes rows the SPA is still
			// holding, so the page is now wrong — but the operator may be
			// mid-inspection of exactly that wrongness, and reloading it out
			// from under them is destroying evidence to tidy up.
			e.printf("note: a page is attached — run 'ao-harness ui reload'\n")
		}
		return nil
	})
}

// pageIsAttached is a cheap yes/no over the bridge. Any failure answers
// "no": this is a note, and a note that could fail a command would be
// worse than the note is worth.
func pageIsAttached(ctx context.Context, e *env, client *harnessclient.Client) bool {
	probeCtx, cancel := context.WithTimeout(ctx, bridgeProbeTimeout)
	defer cancel()
	_, err := e.queryUI(probeCtx, client, map[string]any{"kind": "element", "selector": "body"})
	return err == nil
}

func runThreads(e *env, args []string) error {
	flags := e.newFlagSet("threads")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("threads takes no positional arguments (got %v)", rest)
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		rows, raw, err := listThreadRows(ctx, client)
		if err != nil {
			return err
		}
		if e.jsonOutput() {
			return e.writeRawJSON(raw)
		}
		if len(rows) == 0 {
			e.printf("no threads\n")
			return nil
		}
		table := make([][]string, 0, len(rows))
		for i, row := range rows {
			// The index is the point of the column: `--thread #3` is what an
			// operator can retype, and a uuid is not.
			table = append(table, []string{
				fmt.Sprintf("#%d", i+1), row.ID, row.Provider,
				truncate(row.Title, 40), truncate(row.WorkspacePath, 48),
			})
		}
		return e.table([]string{"#", "ID", "PROVIDER", "TITLE", "WORKSPACE"}, table)
	})
}

// itemRow is the printed subset of store.Item, for the same reason
// threadRow is.
type itemRow struct {
	ID        string `json:"id"`
	TurnIndex int    `json:"turnIndex"`
	ItemIndex int    `json:"itemIndex"`
	Kind      string `json:"kind"`
	Role      string `json:"role"`
	Status    string `json:"status"`
	Summary   string `json:"summary"`
}

func runItems(e *env, args []string) error {
	flags := e.newFlagSet("items [--thread <id|#N|last|title-prefix>]")
	thread := flags.String("thread", "", "thread selector: id, #N from `threads`, `last`, or a unique title prefix")
	turn := flags.Int("turn", -1, "only items in this turn index")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if *thread == "" && len(rest) == 1 {
		*thread = rest[0]
		rest = nil
	}
	if len(rest) != 0 {
		return usagef("items takes only --thread (got %v)", rest)
	}
	if *thread == "" {
		return usagef("items needs --thread <id|#N|last|title-prefix>")
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		// Resolve FIRST. A garbage selector used to reach ListItems, come
		// back empty, and print "no items" — which reads as "that thread is
		// empty" rather than "that thread does not exist".
		row, err := resolveThreadSelector(ctx, client, *thread)
		if err != nil {
			return err
		}
		return e.printItems(ctx, client, row.ID, *turn)
	})
}

func (e *env) printItems(ctx context.Context, client *harnessclient.Client, threadID string, turn int) error {
	// true: ask for inline previews so the printed rows carry the complete
	// payload metadata rather than the wire projection's elided copy
	// (internal/itemwire) — this is an inspection tool, not a renderer.
	raw, err := client.Call(ctx, "ListItems", threadID, true)
	if err != nil {
		return err
	}
	var rows []itemRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return fmt.Errorf("decode items: %w", err)
	}
	if turn >= 0 {
		filtered := rows[:0]
		for _, row := range rows {
			if row.TurnIndex == turn {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	if e.jsonOutput() {
		// A turn filter changes the answer, so json follows the filtered
		// set rather than replaying the server's bytes.
		if turn >= 0 {
			return e.writeJSON(rows)
		}
		return e.writeRawJSON(raw)
	}
	if len(rows) == 0 {
		e.printf("no items\n")
		return nil
	}
	table := make([][]string, 0, len(rows))
	for _, row := range rows {
		table = append(table, []string{
			fmt.Sprintf("%d.%d", row.TurnIndex, row.ItemIndex),
			row.Kind, row.Role, row.Status, truncate(row.Summary, 60),
		})
	}
	return e.table([]string{"TURN.ITEM", "KIND", "ROLE", "STATUS", "SUMMARY"}, table)
}

// defaultSendWaitTimeout bounds `send --wait`. A mocked turn finishes in
// well under a second; a minute is long enough for the slowest scripted
// scenario and short enough that a wedged pipeline fails while the
// caller is still watching.
const defaultSendWaitTimeout = 30 * time.Second

func runSend(e *env, args []string) error {
	flags := e.newFlagSet("send --thread <id|#N|last|title-prefix> <text...>")
	thread := flags.String("thread", "", "thread selector: id, #N from `threads`, `last`, or a unique title prefix")
	wait := flags.Bool("wait", false, "block until the turn completes instead of returning once the message is accepted")
	items := flags.Bool("items", false, "with --wait: print the thread's items once the turn completes")
	timeout := flags.Duration("timeout", defaultSendWaitTimeout, "how long --wait blocks")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if *thread == "" {
		return usagef("send needs --thread <id|#N|last|title-prefix>")
	}
	if *items && !*wait {
		return usagef("--items needs --wait (there is nothing to print until the turn closes)")
	}
	text := strings.TrimSpace(strings.Join(rest, " "))
	if text == "" {
		return usagef("send needs message text")
	}

	ctx := context.Background()
	if *wait {
		waitCtx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		ctx = waitCtx
	}
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if err := requireHarnessProtocol(client, capabilityRequirements{Methods: []string{"SendMessage"}}); err != nil {
			return err
		}
		row, err := resolveThreadSelector(ctx, client, *thread)
		if err != nil {
			return err
		}
		if !*wait {
			// nil attachments: SendMessage's third parameter is a []string the
			// server treats as absent when null.
			if _, err := client.Call(ctx, "SendMessage", row.ID, text, nil); err != nil {
				return err
			}
			if e.jsonOutput() {
				return e.writeJSON(map[string]any{"threadId": row.ID, "sent": true})
			}
			e.printf("sent to %s\n", row.ID)
			return nil
		}
		return e.sendAndWait(ctx, client, row, text, *items)
	})
}

// awaitTurnAfterSend sends one message and blocks until the app closes
// that thread's turn. The order is the whole point: subscribe, PARK the
// wait, and only then send. A mock can finish a scripted turn inside the
// SendMessage round trip, so a wait registered after the RPC returns is a
// wait for the NEXT turn.
//
// Shared by `send --wait` and `profile`, which need the identical
// sequence and must not grow two spellings of it — the parked-before-send
// rule is the sort of thing a second copy quietly gets wrong.
func awaitTurnAfterSend(ctx context.Context, client *harnessclient.Client, threadID, text string) (harnessclient.Event, error) {
	channel := string(eventchan.ProviderTurnCompleted)
	if err := client.Subscribe(ctx, channel); err != nil {
		return harnessclient.Event{}, err
	}
	awaiting := client.Await(channel, func(ev harnessclient.Event) bool {
		if ev.Gap {
			return false
		}
		var payload struct {
			ThreadID string `json:"threadId"`
		}
		return json.Unmarshal(ev.Data, &payload) == nil && payload.ThreadID == threadID
	})
	if _, err := client.Call(ctx, "SendMessage", threadID, text, nil); err != nil {
		awaiting.Close()
		return harnessclient.Event{}, err
	}
	event, err := awaiting.Wait(ctx)
	if err != nil {
		return harnessclient.Event{}, fmt.Errorf("the turn on %s never completed: %w", threadID, err)
	}
	return event, nil
}

// sendAndWait is the single-command observe-a-turn path.
func (e *env) sendAndWait(ctx context.Context, client *harnessclient.Client, row threadRow, text string, withItems bool) error {
	event, err := awaitTurnAfterSend(ctx, client, row.ID, text)
	if err != nil {
		return err
	}
	if e.jsonOutput() && !withItems {
		return e.writeJSON(map[string]any{"threadId": row.ID, "sent": true, "completed": json.RawMessage(event.Data)})
	}
	if !e.jsonOutput() {
		e.printf("sent to %s; turn completed\n", row.ID)
	}
	if !withItems {
		return nil
	}
	return e.printItems(ctx, client, row.ID, -1)
}

// readJSONDocument loads a JSON document from a path or stdin ("-"),
// refusing anything that will not parse. Failing here beats a server
// bad_params: the caller still has the file open.
func readJSONDocument(source string) (json.RawMessage, error) {
	var data []byte
	var err error
	if source == "-" {
		data, err = io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
	} else {
		data, err = os.ReadFile(source)
		if err != nil {
			return nil, err
		}
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("%s does not contain valid JSON%s", sourceName(source), jsonErrorPosition(data))
	}
	return json.RawMessage(data), nil
}

// jsonErrorPosition names where a JSON document broke, as a suffix for
// the "does not contain valid JSON" error. json.Valid answers only a
// bool, so the position comes from a second decode; the cost is paid
// only on the failure path, where the caller is about to open an editor
// and needs a line number far more than a nanosecond.
func jsonErrorPosition(data []byte) string {
	var v any
	err := json.Unmarshal(data, &v)
	var syn *json.SyntaxError
	if !errors.As(err, &syn) {
		return ""
	}
	line, col := 1, 1
	for _, b := range data[:min(int(syn.Offset), len(data))] {
		if b == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return fmt.Sprintf(" (%v at line %d, column %d, byte offset %d)", syn, line, col, syn.Offset)
}

func sourceName(source string) string {
	if source == "-" {
		return "stdin"
	}
	return source
}
