package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"agent-overflow/internal/agenthome"
)

const (
	// Label names the delivery's hook and its receipts
	// (store.SessionStartReceipts), and opens the warning a provider shows
	// when the delivery did not arrive.
	Label = "Agent Overflow memory"
	// Heading opens every delivery and Ending closes it, so a provider can
	// tell this hook's output from another SessionStart hook's.
	Heading = "## Agent Overflow memory"
	Ending  = "[End of Agent Overflow memory]"
	// Budget bounds a delivery, its stamp included, in UTF-16 code units:
	// Claude counts them for the 10,000 characters of one hook's output it
	// keeps, and Codex holds hook output to the same 10,000.
	Budget = 9000
	// instructionsKeep bounds the cleaned bytes of instructions.md a
	// delivery reads. A UTF-16 unit takes at most three bytes of UTF-8, so
	// more would not fit.
	instructionsKeep = 3 * Budget
	// maxListedProblems bounds the warnings about single files a delivery
	// lists; the rest are counted.
	maxListedProblems = 20
	// noFacts is the body of an index whose folder holds no facts.
	noFacts = "No facts yet."
)

// StampPrefix opens the line under the heading that carries a delivery's
// time (Stamp). Claude Code leaves out a resumed session's SessionStart
// context whose text its transcript already holds
// (docs/references/claude-wire.md, SessionStart hooks), so memory changed
// back to a copy the session had before would not reach the model; the time
// makes each delivery its own text, and tells claude-tui which run a context
// in a model request came from (provider.SessionStartHook.Stamp).
const StampPrefix = "[Delivered at "

// stampLayout is the delivery's time in a stamp: RFC 3339 in UTC, to the
// millisecond, so two runs a moment apart differ, and always as long.
const stampLayout = "2006-01-02T15:04:05.000Z07:00"

// stampLine is the stamp of a delivery at at.
func stampLine(at time.Time) string {
	return StampPrefix + at.UTC().Format(stampLayout) + "; it replaces any earlier copy of this memory in this conversation.]"
}

// stampSize is the size of every stamp line with the line break before it.
var stampSize = 1 + utf16Len(stampLine(time.Unix(0, 0)))

// Stamp is a delivery Build made with the stamp of a delivery at at under
// its heading.
func Stamp(text string, at time.Time) string {
	return Heading + "\n" + stampLine(at) + strings.TrimPrefix(text, Heading)
}

// Digest identifies a delivery: the SHA-256 of its text without its stamp,
// in hex, so a delivery and a Build of the same files agree.
func Digest(text string) string {
	sum := sha256.Sum256([]byte(withoutStamp(text)))
	return hex.EncodeToString(sum[:])
}

// withoutStamp is text without the stamp under its heading. Build follows the
// heading with a blank line, so a second line that opens with StampPrefix is
// always a stamp.
func withoutStamp(text string) string {
	heading, rest, ok := strings.Cut(text, "\n")
	if !ok || !strings.HasPrefix(rest, StampPrefix) {
		return text
	}
	_, rest, ok = strings.Cut(rest, "\n")
	if !ok {
		return text
	}
	return heading + "\n" + rest
}

// Build is plan's delivery as of now, without its stamp, and the failures to
// read it other than absence, which the caller reports. The text depends on
// the folders' files and the local date alone, so two builds of unchanged
// folders on one day are the same bytes.
func Build(plan Plan, now time.Time) (string, []error) {
	if err := plan.validate(); err != nil {
		return "", []error{err}
	}
	d := readDelivery(plan, now)
	d.fit()
	return d.render(), d.errs
}

const (
	// projectNameRunes bounds a project's name as a delivery shows it.
	projectNameRunes = 200
	// markerReadBytes bounds what a delivery reads of ProjectPathFile.
	markerReadBytes = 4 << 10
)

// readDelivery reads every block of plan's delivery, nothing cut.
func readDelivery(plan Plan, now time.Time) *delivery {
	d := &delivery{assistant: plan.Home != "", today: now.Format(dateLayout)}
	d.user = d.readIndex("### User memory ("+folderShown(plan.User)+")", plan.User, func() (*os.Root, error) { return openFolder(plan.User) })
	writesProject := false
	switch len(plan.Projects) {
	case 0:
	case 1:
		project := plan.Projects[0]
		heading := "### Project memory: " + projectName(project) + " (" + folderShown(project.Dir) + ")"
		if off := d.clash(project); off != "" {
			d.projectOff = heading + "\n" + off
			break
		}
		d.project = d.readIndex(heading, project.Dir, func() (*os.Root, error) { return openFolder(project.Dir) })
		writesProject = true
	default:
		d.projects = d.listProjects(plan.Projects)
		writesProject = true
	}
	if d.assistant {
		d.instructions = readInstructionsBlock(plan.Home, &d.errs)
		home := HomeDir(plan.Home)
		d.home = d.readIndex("### Your memory ("+folderShown(home)+")", home, func() (_ *os.Root, err error) {
			root, err := openFolder(plan.Home)
			if err != nil {
				return nil, err
			}
			defer closeInto(root, &err)
			return openFolderIn(root, homeFolder)
		})
	}
	if !plan.ReadOnly {
		d.rules = writeRules(d.assistant, writesProject)
	}
	d.measure()
	return d
}

// delivery is one Build: its blocks, in delivery order, and the cut that
// holds it to Budget.
type delivery struct {
	assistant bool
	today     string
	// rules are the write rules, "" for a session that may not write.
	rules string
	user  *index
	// project is the one project's index; projectOff replaces it when the
	// project's folder belongs to another project; projects lists an
	// assistant's projects.
	project      *index
	projectOff   string
	projects     *projectList
	instructions *instructions
	home         *index
	// problems are the maintenance warnings that do not depend on the cut.
	problems []string
	// over is the delivery's size when it is over Budget after every cut.
	over int
	errs []error
	// fixedSize is the size of the frame and the blocks the cut never
	// changes; problemLines are the problems as the warnings list them,
	// and problemsSize their size there (measure).
	fixedSize    int
	problemLines []string
	problemsSize int
}

// index is one scope's index and how much of it the cut keeps.
type index struct {
	heading string
	dir     string
	facts   []fact
	notes   []string
	// floor is the number of context facts, which come first and are never
	// cut; keep is the number of facts shown.
	floor int
	keep  int
	// cum[k] is the size of the lines the first k facts make, type labels
	// included, each with the line break before it; baseSize is the size of
	// the heading, the notes and an empty index's line; dirSize is the size
	// of the folder as moreLine shows it.
	cum      []int
	baseSize int
	dirSize  int
}

// readIndex reads the scope at dir, which open opens, as an index.
func (d *delivery) readIndex(heading, dir string, open func() (*os.Root, error)) *index {
	x := &index{heading: heading, dir: dir}
	defer x.measure()
	read, err := readScope(dir, d.today, open)
	x.facts = read.facts
	d.problems = append(d.problems, read.problems...)
	if read.more {
		x.notes = append(x.notes, fmt.Sprintf("[Only the first %d entries of %s were read.]", maxEntries, folderShown(dir)))
	}
	if err != nil {
		x.notes = append(x.notes, fmt.Sprintf("[%s could not be read: %v.]", folderShown(dir), err))
		d.errs = append(d.errs, fmt.Errorf("read the memory folder %s: %w", dir, err))
	}
	return x
}

// readScope reads the facts of the scope at dir, which open opens; an
// absent folder holds none. A folder past the entry bound is a problem.
func readScope(dir, today string, open func() (*os.Root, error)) (folderRead, error) {
	root, err := open()
	if errors.Is(err, fs.ErrNotExist) {
		return folderRead{}, nil
	}
	if err != nil {
		return folderRead{}, err
	}
	read, err := readFolder(root, dir, today)
	err = errors.Join(err, root.Close())
	if read.more {
		read.problems = append(read.problems, fmt.Sprintf("%s holds more than %d entries, and only the first %d were read. Merge or delete facts.", folderShown(dir), maxEntries, maxEntries))
	}
	return read, err
}

// measure fills the sizes the cut reads and keeps every fact.
func (x *index) measure() {
	x.cum = make([]int, len(x.facts)+1)
	last := -1
	for i, f := range x.facts {
		size := 1 + utf16Len(factLine(f))
		if f.kind != last {
			size += 1 + utf16Len(factTypes[f.kind].label+":")
			last = f.kind
		}
		x.cum[i+1] = x.cum[i] + size
		if f.kind == contextType {
			x.floor = i + 1
		}
	}
	x.baseSize = utf16Len(x.heading)
	for _, note := range x.notes {
		x.baseSize += 1 + utf16Len(note)
	}
	if len(x.facts) == 0 && len(x.notes) == 0 {
		x.baseSize += 1 + utf16Len(noFacts)
	}
	x.dirSize = utf16Len(folderShown(x.dir))
	x.keep = len(x.facts)
}

// size is the index block's size with keep facts.
func (x *index) size(keep int) int {
	size := x.baseSize + x.cum[keep]
	if cut := len(x.facts) - keep; cut > 0 {
		// The line break, then moreLine: `[`, the count, the words, ` in `,
		// the folder and `]`.
		words := " more facts"
		if cut == 1 {
			words = " more fact"
		}
		size += 1 + 1 + len(strconv.Itoa(cut)) + len(words) + len(" in ") + x.dirSize + 1
	}
	return size
}

func (x *index) render() string {
	var b strings.Builder
	b.WriteString(x.heading)
	if len(x.facts) == 0 && len(x.notes) == 0 {
		b.WriteString("\n" + noFacts)
	}
	last := -1
	for _, f := range x.facts[:x.keep] {
		if f.kind != last {
			b.WriteString("\n" + factTypes[f.kind].label + ":")
			last = f.kind
		}
		b.WriteString("\n" + factLine(f))
	}
	if x.keep < len(x.facts) {
		b.WriteString("\n" + x.moreLine(x.keep))
	}
	for _, note := range x.notes {
		b.WriteString("\n" + note)
	}
	return b.String()
}

// moreLine ends an index the cut left keep facts of.
func (x *index) moreLine(keep int) string {
	return fmt.Sprintf("[%s in %s]", plural(len(x.facts)-keep, "more fact", "more facts"), folderShown(x.dir))
}

// factLine is a fact's line in an index.
func factLine(f fact) string {
	line := "- " + f.description + " (" + f.name
	if f.expires != "" {
		line += "; expires " + f.expires
	}
	if f.until != "" {
		line += "; until " + f.until
	}
	return line + ")"
}

// projectName is a project's name as a delivery shows it.
func projectName(project Project) string {
	return oneLine(agenthome.CleanText(project.Name), projectNameRunes)
}

// clash is what replaces a project's index when its folder records another
// project's path, or a ProjectPathFile that is not a path, "" when it
// records the project's own or none. A clash is a maintenance warning.
func (d *delivery) clash(project Project) string {
	recorded, err := recordedPath(project.Dir)
	marker := filepath.Join(project.Dir, ProjectPathFile)
	switch {
	case errors.Is(err, errBadMarker):
		d.problems = append(d.problems, fmt.Sprintf(
			"Project memory of %s (%s) is off: %s %v. Write the project's path in it, or delete it.",
			projectName(project), project.Path, marker, err))
		return "[Project memory is off for this project: its folder's " + ProjectPathFile + " does not name a project.]"
	case err != nil:
		d.errs = append(d.errs, fmt.Errorf("read %s: %w", marker, err))
		return ""
	case recorded == "" || recorded == filepath.Clean(project.Path):
		return ""
	}
	d.problems = append(d.problems, fmt.Sprintf(
		"Project memory of %s (%s) is off: its folder %s records %s in %s, a project whose path has the same key. Tell the user; renaming either project's folder ends the clash.",
		projectName(project), project.Path, folderShown(project.Dir), recorded, ProjectPathFile))
	return "[Project memory is off for this project: its folder belongs to the project at " + recorded + ".]"
}

// errBadMarker is a ProjectPathFile that does not hold one absolute path.
var errBadMarker = errors.New("does not hold one absolute path on one line")

// recordedPath is the project path a memory folder records, "" when it
// records none or the folder cannot be opened, which the folder's own read
// reports.
func recordedPath(dir string) (_ string, err error) {
	root, err := openFolder(dir)
	if err != nil {
		return "", nil
	}
	defer closeInto(root, &err)
	file, err := agenthome.OpenRegular(root, ProjectPathFile)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer closeInto(file, &err)
	text, whole, err := agenthome.ReadCleaned(file, markerReadBytes)
	if err != nil {
		return "", err
	}
	recorded := strings.TrimSpace(text)
	switch {
	case recorded == "":
		return "", nil
	case !whole, strings.ContainsAny(recorded, "\n\t"), !filepath.IsAbs(recorded):
		return "", errBadMarker
	}
	return filepath.Clean(recorded), nil
}

// projectList is the block that names an assistant's projects, each with its
// memory folder and how many facts it holds, and how many lines the cut
// keeps.
type projectList struct {
	lines []string
	keep  int
	// cum[k] is the size of the first k lines, each with the line break
	// before it; moreDir is the folder the cut line names.
	cum     []int
	moreDir string
}

// projectsHeading opens the project list.
const projectsHeading = "### Your projects\nEach project's facts are in its memory folder, one file each with a description line; read them when you work on the project."

// listProjects lists an assistant's projects.
func (d *delivery) listProjects(projects []Project) *projectList {
	list := &projectList{moreDir: filepath.Dir(projects[0].Dir)}
	for _, project := range projects {
		line := "- " + projectName(project) + " (" + project.Path + "): "
		if off := d.clash(project); off != "" {
			list.lines = append(list.lines, line+off)
			continue
		}
		read, err := readScope(project.Dir, d.today, func() (*os.Root, error) { return openFolder(project.Dir) })
		d.problems = append(d.problems, read.problems...)
		if err != nil {
			d.errs = append(d.errs, fmt.Errorf("read the memory folder %s: %w", project.Dir, err))
			list.lines = append(list.lines, fmt.Sprintf("%s[%s could not be read: %v.]", line, folderShown(project.Dir), err))
			continue
		}
		count := plural(len(read.facts), "fact", "facts")
		if read.more {
			count += fmt.Sprintf(" in its first %d entries", maxEntries)
		}
		list.lines = append(list.lines, line+folderShown(project.Dir)+", "+count)
	}
	list.cum = make([]int, len(list.lines)+1)
	for i, line := range list.lines {
		list.cum[i+1] = list.cum[i] + 1 + utf16Len(line)
	}
	list.keep = len(list.lines)
	return list
}

func (l *projectList) size() int {
	size := utf16Len(projectsHeading) + l.cum[l.keep]
	if l.keep < len(l.lines) {
		size += 1 + utf16Len(l.moreLine())
	}
	return size
}

func (l *projectList) render() string {
	var b strings.Builder
	b.WriteString(projectsHeading)
	for _, line := range l.lines[:l.keep] {
		b.WriteString("\n" + line)
	}
	if l.keep < len(l.lines) {
		b.WriteString("\n" + l.moreLine())
	}
	return b.String()
}

// moreLine ends a project list the cut shortened.
func (l *projectList) moreLine() string {
	return "[" + plural(len(l.lines)-l.keep, "more project; its memory folder is", "more projects; their memory folders are") + " in " + folderShown(l.moreDir) + "]"
}

// instructions is the block of an assistant's instructions.md.
type instructions struct {
	path string
	// body is what the block shows of the file; bytes is the file's size.
	body  string
	bytes int64
	// out replaces the body with a line naming the path, which the cut
	// sets; size and outSize are the block's size either way.
	out     bool
	size    int
	outSize int
}

// readInstructionsBlock reads the home's instructions.md for its block,
// adding a failure other than absence to errs.
func readInstructionsBlock(home string, errs *[]error) *instructions {
	block := &instructions{path: filepath.Join(home, agenthome.InstructionsFile)}
	text, bytes, err := readInstructions(home)
	block.bytes = bytes
	switch {
	case errors.Is(err, fs.ErrNotExist):
		block.body = "[" + agenthome.InstructionsFile + " does not exist yet.]"
	case errors.Is(err, errTooLong):
		block.out = true
	case err != nil:
		block.body = fmt.Sprintf("[%s could not be read: %v. Read it yourself.]", agenthome.InstructionsFile, err)
		*errs = append(*errs, fmt.Errorf("read %s: %w", block.path, err))
	case strings.TrimSpace(text) == "":
		block.body = "[" + agenthome.InstructionsFile + " is empty.]"
	default:
		block.body = strings.TrimRight(text, "\n")
	}
	block.size = utf16Len(block.heading()) + 1 + utf16Len(block.body)
	block.outSize = utf16Len(block.heading()) + 1 + utf16Len(block.outLine())
	return block
}

func (b *instructions) heading() string {
	return "### Your standing instructions (" + b.path + ")"
}

// outLine replaces the body of a file that does not fit.
func (b *instructions) outLine() string {
	return fmt.Sprintf("[%s is %d bytes, more than fits beside your memory. Read it from %s before you act.]",
		agenthome.InstructionsFile, b.bytes, b.path)
}

func (b *instructions) render() string {
	if b.out {
		return b.heading() + "\n" + b.outLine()
	}
	return b.heading() + "\n" + b.body
}

func (b *instructions) currentSize() int {
	if b.out {
		return b.outSize
	}
	return b.size
}

// errTooLong is instructions.md read to its bound.
var errTooLong = errors.New("too long to deliver")

// readInstructions reads the home's instructions.md as cleaned text, with
// its size in bytes.
func readInstructions(home string) (_ string, bytes int64, err error) {
	root, err := openFolder(home)
	if err != nil {
		return "", 0, err
	}
	defer closeInto(root, &err)
	file, err := agenthome.OpenRegular(root, agenthome.InstructionsFile)
	if err != nil {
		return "", 0, err
	}
	defer closeInto(file, &err)
	info, err := file.Stat()
	if err != nil {
		return "", 0, err
	}
	text, whole, err := agenthome.ReadCleaned(file, instructionsKeep)
	if err != nil {
		return "", info.Size(), err
	}
	if !whole {
		return "", info.Size(), errTooLong
	}
	return text, info.Size(), nil
}

// indexes are the delivery's indexes in order, the ones it has.
func (d *delivery) indexes() []*index {
	var out []*index
	for _, x := range []*index{d.user, d.project, d.home} {
		if x != nil {
			out = append(out, x)
		}
	}
	return out
}

// fit holds the delivery to Budget: when it would not fit with every index
// cut to its context facts, instructions.md gives way to its path, and then
// a project list loses projects from its end; then the longest index loses
// facts from its end, one at a time, until it fits.
func (d *delivery) fit() {
	indexes := d.indexes()
	for _, x := range indexes {
		x.keep = x.floor
	}
	if d.instructions != nil && !d.instructions.out && d.size() > Budget {
		d.instructions.out = true
	}
	if list := d.projects; list != nil {
		for list.keep > 0 && d.size() > Budget {
			list.keep--
		}
	}
	for _, x := range indexes {
		x.keep = len(x.facts)
	}
	sizes := make([]int, len(indexes))
	for i, x := range indexes {
		sizes[i] = x.size(x.keep)
	}
	for d.size() > Budget {
		longest := -1
		for i, x := range indexes {
			if x.keep > x.floor && (longest < 0 || sizes[i] > sizes[longest]) {
				longest = i
			}
		}
		if longest < 0 {
			break
		}
		x := indexes[longest]
		x.keep--
		sizes[longest] = x.size(x.keep)
	}
	if size := d.size(); d.assistant && size > Budget {
		d.over = size
	}
}

// measure fills the sizes of what the cut never changes.
func (d *delivery) measure() {
	d.fixedSize = utf16Len(Heading) + stampSize + 2 + utf16Len(Ending)
	for _, block := range []string{d.rules, d.projectOff} {
		if block != "" {
			d.fixedSize += 2 + utf16Len(block)
		}
	}
	d.problemLines = d.problems
	if len(d.problems) > maxListedProblems {
		d.problemLines = append(d.problems[:maxListedProblems:maxListedProblems], fmt.Sprintf("And %d more like these.", len(d.problems)-maxListedProblems))
	}
	d.problemsSize = 0
	for _, line := range d.problemLines {
		d.problemsSize += len(warningsBullet) + utf16Len(line)
	}
}

// blocks are the delivery's blocks in order, rendered.
func (d *delivery) blocks() []string {
	var out []string
	if d.rules != "" {
		out = append(out, d.rules)
	}
	out = append(out, d.user.render())
	switch {
	case d.project != nil:
		out = append(out, d.project.render())
	case d.projectOff != "":
		out = append(out, d.projectOff)
	case d.projects != nil:
		out = append(out, d.projects.render())
	}
	if d.assistant {
		out = append(out, d.instructions.render(), d.home.render())
	}
	if warnings := d.warnings(); warnings != "" {
		out = append(out, warnings)
	}
	return out
}

// size is the stamped delivery's size as the cut stands, without rendering
// it.
func (d *delivery) size() int {
	size := d.fixedSize
	if d.projects != nil {
		size += 2 + d.projects.size()
	}
	if d.instructions != nil {
		size += 2 + d.instructions.currentSize()
	}
	for _, x := range d.indexes() {
		size += 2 + x.size(x.keep)
	}
	if d.assistant {
		lines := d.cutLines()
		if len(lines) > 0 || len(d.problemLines) > 0 {
			size += 2 + utf16Len(warningsHeading) + d.problemsSize
			for _, line := range lines {
				size += len(warningsBullet) + utf16Len(line)
			}
		}
	}
	return size
}

// render is the delivery's text. Text inside it that would read as its
// ending, or as the stamp under its heading, is written with parentheses for
// its brackets, so a provider looking for either finds only the delivery's
// own; the replacement keeps every size.
func (d *delivery) render() string {
	body := strings.Join(d.blocks(), "\n\n")
	body = strings.ReplaceAll(body, Ending, "("+strings.Trim(Ending, "[]")+")")
	body = strings.ReplaceAll(body, StampPrefix, "("+strings.TrimPrefix(StampPrefix, "["))
	return Heading + "\n\n" + body + "\n\n" + Ending
}

const (
	// warningsHeading opens the maintenance block; each warning follows
	// on its own line after warningsBullet.
	warningsHeading = "### Memory maintenance\nKeeping these folders sound is your job. Fix these:"
	warningsBullet  = "\n- "
)

// cutLines are the maintenance warnings the cut makes.
func (d *delivery) cutLines() []string {
	var lines []string
	for _, x := range d.indexes() {
		if cut := len(x.facts) - x.keep; cut > 0 {
			lines = append(lines, fmt.Sprintf("The index of %s was cut to fit this delivery: %s not shown. Merge, shorten or delete facts there.",
				folderShown(x.dir), plural(cut, "fact is", "facts are")))
		}
	}
	if list := d.projects; list != nil && list.keep < len(list.lines) {
		lines = append(lines, fmt.Sprintf("The list of your projects was cut to fit this delivery: %s not listed.",
			plural(len(list.lines)-list.keep, "project is", "projects are")))
	}
	if b := d.instructions; b != nil && b.out {
		lines = append(lines, fmt.Sprintf("%s (%d bytes) did not fit in this delivery beside your memory and was left out. Shorten it.", b.path, b.bytes))
	}
	if d.over > 0 {
		lines = append(lines, fmt.Sprintf("This delivery is still about %d characters after every cut, over the %d it is held to. Shorten your context facts and %s.", d.over, Budget, agenthome.InstructionsFile))
	}
	return lines
}

// warnings is the maintenance block of an assistant's delivery, "" for any
// other or when nothing needs fixing.
func (d *delivery) warnings() string {
	if !d.assistant {
		return ""
	}
	lines := append(d.cutLines(), d.problemLines...)
	if len(lines) == 0 {
		return ""
	}
	return warningsHeading + warningsBullet + strings.Join(lines, warningsBullet)
}

// writeRules are the rules a delivery opens with, naming the scopes its
// session may write (docs/specs/memory.md, "Write rules").
func writeRules(home, project bool) string {
	const user = "the user folder holds only how the user decides, across projects."
	var where []string
	if home {
		where = append(where, "Your memory folder is the default")
	}
	switch {
	case project && home:
		where = append(where, "a project's folder holds facts true of the project, whoever learned them")
	case project:
		where = append(where, "the project's folder holds facts true of the project, whoever learned them")
	}
	where = append(where, user)
	scopes := strings.Join(where, "; ")
	scopes = strings.ToUpper(scopes[:1]) + scopes[1:]
	rules := []string{
		"Save a fact only when a future session would act differently knowing it, it is confirmed (verified, or the user said it), no file, doc or code already holds it, and it outlives the current task. Never save progress, plans, task notes or anything temporary.",
		"Update the fact on the subject instead of adding a near-duplicate. Delete a fact the moment it is shown wrong. When you rely on a fact and it holds, set confirmed to today.",
		scopes,
		"A user's correction is a ruling in the scope it is about, or a user preference when it holds across projects.",
	}
	if project {
		rules = append(rules, "A check that caught a problem, or a miss that got through, is a project verification: how to prove that kind of work.")
	}
	rules = append(rules, "When a human contributor would need it too, propose a change to the repository's docs instead.")
	return `### Keeping memory
Memory is one fact per Markdown file in the folders below, which you edit with your file tools. A fact file opens with:
---
description: one line, which the index shows
type: context, ruling, verification, pitfall, convention, preference or reference
source: who said it, or how it was verified
confirmed: YYYY-MM-DD, the day it was last verified true
expires: YYYY-MM-DD, optional; a context fact needs expires or until
until: an end condition in plain words, optional; delete the fact once it is met
---
- ` + strings.Join(rules, "\n- ")
}

// folderShown is a folder as a delivery names it, with a trailing separator.
func folderShown(dir string) string {
	return filepath.Clean(dir) + string(filepath.Separator)
}

// utf16Len is the length of s in UTF-16 code units.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
