package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/agenthome"
)

const (
	// Label names the delivery in the warnings a provider shows when it
	// did not arrive.
	Label = "Agent Overflow memory"
	// Heading opens every whole delivery, followed by the part it is when
	// the delivery has several (Plan.HeadingOf), and Ending closes it, so a
	// provider can tell this hook's outputs from another SessionStart
	// hook's.
	Heading = "## Agent Overflow memory"
	Ending  = "[End of Agent Overflow memory]"
	// Limit is the most a hook's output can be, in bytes, and reach the
	// model whole: Codex keeps 2,500 tokens it counts as four bytes each,
	// Claude 10,000 UTF-16 code units, which a text never has more of than
	// bytes. Past it each provider saves that output to a file and shows the
	// model its start with the file's path (docs/specs/memory.md, "Size").
	// Each part of a delivery is an output of its own.
	Limit = 10000
	// instructionsKeep bounds the cleaned bytes of instructions.md a
	// delivery reads; a longer file is named with its path instead, and
	// the assistant is warned.
	instructionsKeep = 64 << 10
	// maxListedProblems bounds the warnings about single files a delivery
	// lists; the rest are counted.
	maxListedProblems = 20
	// noFacts is the body of an index whose folder holds no facts.
	noFacts = "No facts yet."
)

// StampPrefix opens the line under the heading that carries a delivery's
// time (Stamp). claude-tui tells by the time which run a context in a model
// request came from (provider.SessionStartHook.Stamp).
const StampPrefix = "[Delivered at "

// stampLayout is the delivery's time in a stamp: RFC 3339 in UTC, to the
// millisecond, so two runs a moment apart differ, and always as long.
const stampLayout = "2006-01-02T15:04:05.000Z07:00"

// stampLine is the stamp of a delivery, or a part of one as noun says, at
// at.
func stampLine(at time.Time, noun string) string {
	return StampPrefix + at.UTC().Format(stampLayout) + "; it replaces any earlier copy of this " + noun + " in this conversation.]"
}

// stampSize is the size of the stamp line of noun with the line break
// before it, the same at every time.
func stampSize(noun string) int {
	return 1 + len(stampLine(time.Unix(0, 0), noun))
}

// Stamp is a part Build made with the stamp of a delivery at at under its
// heading line.
func Stamp(text string, at time.Time) string {
	heading, rest, _ := strings.Cut(text, "\n")
	return heading + "\n" + stampLine(at, headingNoun(heading)) + "\n" + rest
}

// headingNoun is what a delivery under heading calls itself: the memory,
// or a part of it.
func headingNoun(heading string) string {
	if heading == Heading {
		return "memory"
	}
	return "part"
}

// Output is one part of a delivery as Build reads it.
type Output struct {
	Part Part
	// Name says what the part holds, as a warning row names it.
	Name string
	// Text is the part's whole delivery without its stamp, and Size its
	// size once stamped, as the provider receives it.
	Text string
	Size int
}

// Over reports whether the provider shows the model only the part's start.
func (o Output) Over() bool { return o.Size > Limit }

// Build is plan's delivery as of now, each part as its hook output carries
// it without its stamp, and the failures to read it other than absence,
// which the caller reports. The texts depend on the folders' files and the
// local date alone, so two builds of unchanged folders on one day are the
// same bytes. Nothing is cut: a part over Limit says so at its top, where
// the start a provider shows carries it.
func Build(plan Plan, now time.Time) ([]Output, []error) {
	if err := plan.validate(); err != nil {
		return nil, []error{err}
	}
	d := readDelivery(plan, now, "")
	outputs := make([]Output, len(d.parts))
	for i, part := range d.parts {
		text := d.render(part)
		outputs[i] = Output{Part: part, Name: d.nameOf(part), Text: text, Size: len(text) + stampSize(d.noun)}
	}
	return outputs, d.errs
}

const (
	// projectNameRunes bounds a project's name as a delivery shows it.
	projectNameRunes = 200
	// markerReadBytes bounds what a delivery reads of ProjectPathFile.
	markerReadBytes = 4 << 10
)

// readDelivery reads the blocks of plan's delivery that part holds, every
// block for "": the warnings part reads them all too, since it names each
// part over Limit.
func readDelivery(plan Plan, now time.Time, part Part) *delivery {
	d := &delivery{assistant: plan.Home != "", day: readDayOf(now), parts: plan.Parts(), plan: plan}
	d.noun = headingNoun(plan.HeadingOf(d.parts[0]))
	reads := func(p Part) bool { return part == "" || part == PartWarnings || part == p }
	if reads(PartInstructions) && d.assistant {
		d.instructions = readInstructionsBlock(plan.Home, &d.errs)
	}
	if reads(PartHome) && d.assistant {
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
	if reads(PartProject) {
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
		default:
			d.projects = d.listProjects(plan.Projects)
		}
	}
	if reads(PartUser) {
		d.user = d.readIndex("### User memory ("+folderShown(plan.User)+")", plan.User, func() (*os.Root, error) { return openFolder(plan.User) })
		if !plan.ReadOnly {
			d.rules = writeRules(d.assistant, writesProject(plan))
		}
	}
	// A part over Limit opens with the notice of its size, which states the
	// size of the stamped part with the notice itself included. The
	// warnings name the other parts over it, so they are measured first.
	d.over = map[Part]int{}
	for _, p := range d.parts {
		if p != PartWarnings && reads(p) {
			d.measure(p)
		}
	}
	if reads(PartWarnings) && d.assistant {
		d.measure(PartWarnings)
	}
	return d
}

// writesProject reports whether a session of plan may write a project's
// folder, which its write rules then name: an assistant's projects, or the
// one project of a thread when the project owns its folder (clash).
func writesProject(plan Plan) bool {
	switch len(plan.Projects) {
	case 0:
		return false
	case 1:
		return ownerOf(plan.Projects[0]).owns(plan.Projects[0])
	}
	return true
}

// measure records part's stamped size with its notice when it is over
// Limit without one.
func (d *delivery) measure(part Part) {
	if size := len(d.text(part, d.sectionsOf(part))) + stampSize(d.noun); size > Limit {
		d.over[part] = noticedSize(size, len("\n\n"), d.noun)
	}
}

// delivery is what one hook run or Build read: the blocks of the parts it
// reads.
type delivery struct {
	plan      Plan
	parts     []Part
	assistant bool
	day       readDay
	// noun is what each part calls itself in its stamp and its notice:
	// "memory" when it is the delivery's one part, "part" otherwise.
	noun string
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
	// problems are the maintenance warnings about the folders' files.
	problems []string
	// over holds each part over Limit with its stamped size, its notice
	// included.
	over map[Part]int
	errs []error
}

// index is one scope's index.
type index struct {
	heading string
	dir     string
	facts   []fact
	notes   []string
}

// readIndex reads the scope at dir, which open opens, as an index.
func (d *delivery) readIndex(heading, dir string, open func() (*os.Root, error)) *index {
	x := &index{heading: heading, dir: dir}
	read, err := readScope(dir, d.day, open)
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
func readScope(dir string, on readDay, open func() (*os.Root, error)) (folderRead, error) {
	root, err := open()
	if errors.Is(err, fs.ErrNotExist) {
		return folderRead{}, nil
	}
	if err != nil {
		return folderRead{}, err
	}
	read, err := readFolder(root, dir, on)
	err = errors.Join(err, root.Close())
	if read.more {
		read.problems = append(read.problems, fmt.Sprintf("%s holds more than %d entries, and only the first %d were read. Merge or delete facts.", folderShown(dir), maxEntries, maxEntries))
	}
	return read, err
}

// render is the index as a whole delivery shows it: its facts under their
// types' labels, context first.
func (x *index) render() string {
	var b strings.Builder
	b.WriteString(x.heading)
	if len(x.facts) == 0 && len(x.notes) == 0 {
		b.WriteString("\n" + noFacts)
	}
	last := -1
	for _, f := range x.facts {
		if f.kind != last {
			b.WriteString("\n" + factTypes[f.kind].label + ":")
			last = f.kind
		}
		b.WriteString("\n" + factLine(f))
	}
	for _, note := range x.notes {
		b.WriteString("\n" + note)
	}
	return b.String()
}

// section is the index as a section of id, its facts keyed by file name and
// each line naming its type, as a change shows it.
func (x *index) section(id string) section {
	s := section{ID: id, Heading: x.heading, Notes: strings.Join(x.notes, "\n"), Keyed: true, text: x.render()}
	for _, f := range x.facts {
		s.Entries = append(s.Entries, entry{Key: f.name, Line: "- " + factTypes[f.kind].name + ": " + strings.TrimPrefix(factLine(f), "- ")})
	}
	return s
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
	if f.confirmed != "" {
		line += "; unchecked since " + f.confirmed
	}
	return line + ")"
}

// projectName is a project's name as a delivery shows it.
func projectName(project Project) string {
	return oneLine(agenthome.CleanText(project.Name), projectNameRunes)
}

// clash is what replaces a project's index when the project does not own
// its folder (ownerOf), "" when it does. It is a maintenance warning, and a
// marker that could not be read is a failure the caller reports as well.
func (d *delivery) clash(project Project) string {
	owner := ownerOf(project)
	marker := filepath.Join(project.Dir, ProjectPathFile)
	switch {
	case owner.folderErr != nil:
		d.errs = append(d.errs, fmt.Errorf("read the memory folder %s: %w", project.Dir, owner.folderErr))
		return fmt.Sprintf("[%s could not be read: %v.]", folderShown(project.Dir), owner.folderErr)
	case errors.Is(owner.err, errBadMarker):
		d.problems = append(d.problems, fmt.Sprintf(
			"Project memory of %s (%s) is off: %s %v. Write the project's path in it, or delete it.",
			projectName(project), project.Path, marker, owner.err))
		return "[Project memory is off for this project: its folder's " + ProjectPathFile + " does not name a project.]"
	case owner.err != nil:
		d.errs = append(d.errs, fmt.Errorf("read %s: %w", marker, owner.err))
		d.problems = append(d.problems, fmt.Sprintf(
			"Project memory of %s (%s) is off: %s could not be read (%v), so the folder may belong to another project. Make it a file holding the project's path, or delete it.",
			projectName(project), project.Path, marker, owner.err))
		return "[Project memory is off for this project: its folder's " + ProjectPathFile + " could not be read.]"
	case owner.owns(project):
		return ""
	}
	d.problems = append(d.problems, fmt.Sprintf(
		"Project memory of %s (%s) is off: its folder %s records %s in %s, a project whose path has the same key. Tell the user; renaming either project's folder ends the clash.",
		projectName(project), project.Path, folderShown(project.Dir), owner.recorded, ProjectPathFile))
	return "[Project memory is off for this project: its folder belongs to the project at " + owner.recorded + ".]"
}

// projectOwner is whose a project memory folder is, by its ProjectPathFile.
// Distinct project paths can share a folder key, so a folder whose marker
// cannot be read is nobody's: neither a delivery nor a session's writable
// roots include it.
type projectOwner struct {
	// recorded is the path the marker holds, "" when the folder or its
	// marker does not exist or err is set.
	recorded string
	// folderErr is the failure to open the folder, which then holds
	// nothing to deliver either.
	folderErr error
	// err is why the marker could not be read as one path: errBadMarker,
	// or the failure to read it.
	err error
}

// ownerOf reads the owner of project's folder.
func ownerOf(project Project) projectOwner {
	root, err := openFolder(project.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return projectOwner{}
	}
	if err != nil {
		return projectOwner{folderErr: err}
	}
	recorded, err := recordedPath(root)
	if closeErr := root.Close(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	return projectOwner{recorded: recorded, err: err}
}

// owns reports whether a delivery may read the folder as project's: its
// marker holds project's own path, or there is none yet, which Prepare
// writes.
func (o projectOwner) owns(project Project) bool {
	return o.folderErr == nil && o.err == nil && (o.recorded == "" || o.recorded == filepath.Clean(project.Path))
}

// names reports whether the folder's marker holds project's own path, which
// a session's write access to the folder needs.
func (o projectOwner) names(project Project) bool {
	return o.owns(project) && o.recorded != ""
}

// errBadMarker is a ProjectPathFile that does not hold one absolute path.
var errBadMarker = errors.New("does not hold one absolute path on one line")

// recordedPath is the project path the memory folder root records, "" when
// its marker does not exist. Prepare publishes a marker whole, so an empty
// or blank one is errBadMarker.
func recordedPath(root *os.Root) (_ string, err error) {
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
	case recorded == "", !whole, strings.ContainsAny(recorded, "\n\t"), !filepath.IsAbs(recorded):
		return "", errBadMarker
	}
	return filepath.Clean(recorded), nil
}

// projectList is the block that names an assistant's projects, each with its
// memory folder and how many facts it holds.
type projectList struct {
	lines []projectLine
}

// projectLine is one project's line, keyed by the project's path.
type projectLine struct {
	path, text string
}

// projectsHeading opens the project list.
const projectsHeading = "### Your projects\nEach project's facts are in its memory folder, one file each with a description line; read them when you work on the project."

// listProjects lists an assistant's projects.
func (d *delivery) listProjects(projects []Project) *projectList {
	list := &projectList{}
	for _, project := range projects {
		line := "- " + projectName(project) + " (" + project.Path + "): "
		if off := d.clash(project); off != "" {
			list.lines = append(list.lines, projectLine{project.Path, line + off})
			continue
		}
		read, err := readScope(project.Dir, d.day, func() (*os.Root, error) { return openFolder(project.Dir) })
		d.problems = append(d.problems, read.problems...)
		if err != nil {
			d.errs = append(d.errs, fmt.Errorf("read the memory folder %s: %w", project.Dir, err))
			list.lines = append(list.lines, projectLine{project.Path, fmt.Sprintf("%s[%s could not be read: %v.]", line, folderShown(project.Dir), err)})
			continue
		}
		count := plural(len(read.facts), "fact", "facts")
		if read.more {
			count += fmt.Sprintf(" in its first %d entries", maxEntries)
		}
		list.lines = append(list.lines, projectLine{project.Path, line + folderShown(project.Dir) + ", " + count})
	}
	return list
}

func (l *projectList) render() string {
	var b strings.Builder
	b.WriteString(projectsHeading)
	for _, line := range l.lines {
		b.WriteString("\n" + line.text)
	}
	return b.String()
}

// section is the list as a section, its lines keyed by project path.
func (l *projectList) section() section {
	s := section{ID: "projects", Heading: projectsHeading, Keyed: true, text: l.render()}
	for _, line := range l.lines {
		s.Entries = append(s.Entries, entry{Key: line.path, Line: line.text})
	}
	return s
}

// instructions is the block of an assistant's instructions.md.
type instructions struct {
	path string
	// body is what the block shows of the file; bytes is the file's size.
	body  string
	bytes int64
	// out replaces the body with a line naming the path, for a file over
	// instructionsKeep.
	out bool
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
	return block
}

func (b *instructions) heading() string {
	return "### Your standing instructions (" + b.path + ")"
}

func (b *instructions) render() string {
	if b.out {
		return b.heading() + "\n" + fmt.Sprintf("[%s is %d bytes, more than a delivery carries. Read it from %s before you act.]",
			agenthome.InstructionsFile, b.bytes, b.path)
	}
	return b.heading() + "\n" + b.body
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

// sectionsOf are part's sections in order (Plan.Parts).
func (d *delivery) sectionsOf(part Part) []section {
	var out []section
	switch part {
	case PartInstructions:
		out = append(out, wholeSection("instructions", d.instructions.render()))
	case PartHome:
		out = append(out, d.home.section("home"))
	case PartProject:
		switch {
		case d.project != nil:
			out = append(out, d.project.section("project"))
		case d.projectOff != "":
			out = append(out, wholeSection("project", d.projectOff))
		case d.projects != nil:
			out = append(out, d.projects.section())
		}
	case PartUser:
		out = append(out, d.user.section("user"))
		if d.rules != "" {
			out = append(out, wholeSection("rules", d.rules))
		}
	case PartWarnings:
		out = append(out, wholeSection("warnings", d.warnings()))
	}
	for i := range out {
		out[i] = out[i].escaped()
	}
	return out
}

// nameOf says what part holds, as a warning row names it.
func (d *delivery) nameOf(part Part) string {
	switch part {
	case PartInstructions:
		return agenthome.InstructionsFile
	case PartHome:
		return "the assistant's own memory index"
	case PartProject:
		if d.projects != nil {
			return "the list of the assistant's projects"
		}
		return "the project memory index"
	case PartUser:
		return "the user memory index"
	}
	return "the memory maintenance warnings"
}

// render is part's text: opened by the notice of its size when it is over
// Limit.
func (d *delivery) render(part Part) string {
	sections := d.sectionsOf(part)
	if size, over := d.over[part]; over {
		sections = append([]section{{text: overNotice(size, d.noun)}}, sections...)
	}
	return d.text(part, sections)
}

// noticedSize is the size of a text of size bytes once it carries its
// overNotice for noun, joined to it by sep more bytes: the notice's digits
// count.
func noticedSize(size, sep int, noun string) int {
	total := size
	// Each pass can add one digit; the digits hold by the third.
	for range 3 {
		total = size + sep + len(overNotice(total, noun))
	}
	return total
}

// overNotice opens a text of size bytes over Limit, which calls itself
// noun.
func overNotice(size int, noun string) string {
	return fmt.Sprintf("[This %s is %d bytes, more than the %d a hook's output reaches you whole. If you see only its start, read all of it from the file the provider saved it to before you act.]", noun, size, Limit)
}

// text is sections as part's whole delivery: under its heading, closed by
// the ending.
func (d *delivery) text(part Part, sections []section) string {
	blocks := make([]string, len(sections))
	for i, s := range sections {
		blocks[i] = s.text
	}
	return d.plan.HeadingOf(part) + "\n\n" + strings.Join(blocks, "\n\n") + "\n\n" + Ending
}

const (
	// warningsHeading opens the maintenance block, then warningsFix and
	// each warning on its own line after warningsBullet, or noWarnings.
	warningsHeading = "### Memory maintenance"
	warningsFix     = "\nKeeping these folders sound is your job. Fix these:"
	warningsBullet  = "\n- "
	noWarnings      = "[Nothing here needs fixing now.]"
)

// The fixes the warnings name for a part over Limit.
const (
	fixInstructions = "Move detail into files in your home that it points to."
	fixIndex        = "Merge or delete facts, or move detail from their descriptions into their bodies."
)

// warnings is the maintenance block of an assistant's delivery. A warning
// that a part is over Limit names no size, so the block reads the same
// while it stays over.
func (d *delivery) warnings() string {
	lines := d.oversizeWarnings()
	if b := d.instructions; b != nil && b.out {
		lines = append(lines, fmt.Sprintf("%s (%d bytes) is more than the %d a delivery carries, so it is named instead of shown. %s", b.path, b.bytes, instructionsKeep, fixInstructions))
	}
	problems := d.problems
	if len(problems) > maxListedProblems {
		problems = append(problems[:maxListedProblems:maxListedProblems], fmt.Sprintf("And %d more like these.", len(d.problems)-maxListedProblems))
	}
	lines = append(lines, problems...)
	if len(lines) == 0 {
		return warningsHeading + "\n" + noWarnings
	}
	return warningsHeading + warningsFix + warningsBullet + strings.Join(lines, warningsBullet)
}

// oversizeWarnings name each part other than the warnings that is over
// Limit, and what shortens it.
func (d *delivery) oversizeWarnings() []string {
	var lines []string
	over := func(what, fix string) {
		line := fmt.Sprintf("%s is more than the %d bytes a hook's output reaches every provider's model whole, so a session may see only its start until it reads the file the provider saved it to.", what, Limit)
		if fix != "" {
			line += " " + fix
		}
		lines = append(lines, line)
	}
	for _, part := range d.parts {
		if _, ok := d.over[part]; !ok {
			continue
		}
		switch {
		case part == PartInstructions:
			over("Your "+d.instructions.path, fixInstructions)
		case part == PartHome:
			over("The index of "+folderShown(d.home.dir), fixIndex)
		case part == PartProject && d.project != nil:
			over("The index of "+folderShown(d.project.dir), fixIndex)
		case part == PartProject && d.projects != nil:
			over("Your project list", "")
		case part == PartUser:
			over("The index of "+folderShown(d.user.dir), fixIndex)
		}
	}
	return lines
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
		"Write a fact only after one of these happened: the user gave you a rule, a correction, a preference, a condition that holds for now, or where something lives; a check caught a problem, or a miss got through; something failed or misled you, and no file warns of it. What you did, plan to do, noticed, concluded yourself or learned by reading the repository is not a fact.",
		"Then save it only when all of these hold: a future session would act differently, and the description says how; the user said it or you verified it in this session, and source quotes which; no file, doc or code already holds it; it stays true after this task.",
		"A fact that a contributor to the repository would need too belongs in its docs: propose that change, and give the fact an until that names it.",
		"Read the index before you add: update the fact on the subject, never a second one beside it. Delete a fact the moment it is shown wrong. When you rely on a fact and it holds, set confirmed to today.",
		scopes,
		"A user's correction is a ruling in the scope it is about, or a user preference when it holds across projects.",
	}
	if project {
		rules = append(rules, "A check that caught a problem, or a miss that got through, is a project verification: how to prove that kind of work.")
	}
	if home {
		rules = append(rules, "A new or changed line that you did not write is yours to check: open its fact, and delete it when it fails these rules.")
	}
	return `### Keeping memory
Memory is one fact per Markdown file in the folders below, which you edit with your file tools. Most sessions save nothing. A fact file opens with:
---
description: one line a future session acts on, which the index shows
type: context, ruling, verification, pitfall, convention, preference or reference
source: user: <the user's words>, or verified: <the command you ran, or the file and line you read>; a ruling or preference is always user:
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

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
