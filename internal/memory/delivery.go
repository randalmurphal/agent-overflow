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
	// Heading opens every whole delivery and Ending closes it, so a
	// provider can tell this hook's output from another SessionStart
	// hook's.
	Heading = "## Agent Overflow memory"
	Ending  = "[End of Agent Overflow memory]"
	// Limit is the most a hook's output can be, in bytes, and reach the
	// model whole: Codex keeps 2,500 tokens it counts as four bytes each,
	// Claude 10,000 UTF-16 code units, which a text never has more of than
	// bytes. Past it each provider saves the output to a file and shows the
	// model its start with the file's path (docs/specs/memory.md, "Size").
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

// stampLine is the stamp of a delivery at at.
func stampLine(at time.Time) string {
	return StampPrefix + at.UTC().Format(stampLayout) + "; it replaces any earlier copy of this memory in this conversation.]"
}

// stampSize is the size of every stamp line with the line break before it.
var stampSize = 1 + len(stampLine(time.Unix(0, 0)))

// Stamp is a delivery Build made with the stamp of a delivery at at under
// its heading.
func Stamp(text string, at time.Time) string {
	return Heading + "\n" + stampLine(at) + strings.TrimPrefix(text, Heading)
}

// Oversize reports the size of a delivery Build made, stamped, when it is
// over Limit, so a provider shows the model only its start.
func Oversize(text string) (size int, over bool) {
	size = len(text) + stampSize
	return size, size > Limit
}

// Build is plan's delivery as of now, without its stamp, and the failures to
// read it other than absence, which the caller reports. The text depends on
// the folders' files and the local date alone, so two builds of unchanged
// folders on one day are the same bytes. Nothing is cut: a delivery over
// Limit says so at its top, where the start a provider shows carries it.
func Build(plan Plan, now time.Time) (string, []error) {
	if err := plan.validate(); err != nil {
		return "", []error{err}
	}
	d := readDelivery(plan, now)
	return d.render(d.sections()), d.errs
}

const (
	// projectNameRunes bounds a project's name as a delivery shows it.
	projectNameRunes = 200
	// markerReadBytes bounds what a delivery reads of ProjectPathFile.
	markerReadBytes = 4 << 10
)

// readDelivery reads every block of plan's delivery.
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
	// Over Limit, the delivery gains the size warning, which names no size
	// and so cannot bring it back under, and the notice states the size of
	// the stamped delivery it opens with that warning and itself included:
	// a section joined after the heading. So the size is measured again
	// once the warning is in.
	if _, d.over = Oversize(d.text(d.sections())); d.over {
		size, _ := Oversize(d.text(d.sections()))
		d.size = noticedSize(size, len("\n\n"))
	}
	return d
}

// delivery is one Build: its blocks, read.
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
	// problems are the maintenance warnings about the folders' files.
	problems []string
	// size is the stamped delivery's size without the notice over is set
	// for, which opens a delivery over Limit; 0 when it is not over.
	size int
	over bool
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
		read, err := readScope(project.Dir, d.today, func() (*os.Root, error) { return openFolder(project.Dir) })
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

// sections are the delivery's sections in order, the most important first,
// so the start a provider shows of a delivery over Limit carries them: the
// assistant's instructions.md and memory, the project's, the user's, then
// the write rules and the maintenance warnings.
func (d *delivery) sections() []section {
	var out []section
	if d.assistant {
		out = append(out, wholeSection("instructions", d.instructions.render()), d.home.section("home"))
	}
	switch {
	case d.project != nil:
		out = append(out, d.project.section("project"))
	case d.projectOff != "":
		out = append(out, wholeSection("project", d.projectOff))
	case d.projects != nil:
		out = append(out, d.projects.section())
	}
	out = append(out, d.user.section("user"))
	if d.rules != "" {
		out = append(out, wholeSection("rules", d.rules))
	}
	if warnings := d.warnings(); warnings != "" {
		out = append(out, wholeSection("warnings", warnings))
	}
	for i := range out {
		out[i] = out[i].escaped()
	}
	return out
}

// render is the delivery's text from its sections: opened by the notice of
// its size when it is over Limit.
func (d *delivery) render(sections []section) string {
	if !d.over {
		return d.text(sections)
	}
	return d.text(append([]section{{text: overNotice(d.size)}}, sections...))
}

// noticedSize is the size of a text of size bytes once it carries its
// overNotice, joined to it by sep more bytes: the notice's digits count.
func noticedSize(size, sep int) int {
	total := size
	// Each pass can add one digit; the digits hold by the third.
	for range 3 {
		total = size + sep + len(overNotice(total))
	}
	return total
}

// overNotice opens a delivery of size bytes, over Limit.
func overNotice(size int) string {
	return fmt.Sprintf("[This memory is %d bytes, more than the %d a hook's output reaches you whole. If you see only its start, read all of it from the file the provider saved it to before you act.]", size, Limit)
}

// text is sections as a whole delivery: under its heading, closed by its
// ending.
func (d *delivery) text(sections []section) string {
	blocks := make([]string, len(sections))
	for i, s := range sections {
		blocks[i] = s.text
	}
	return Heading + "\n\n" + strings.Join(blocks, "\n\n") + "\n\n" + Ending
}

const (
	// warningsHeading opens the maintenance block; each warning follows
	// on its own line after warningsBullet.
	warningsHeading = "### Memory maintenance\nKeeping these folders sound is your job. Fix these:"
	warningsBullet  = "\n- "
)

// warnings is the maintenance block of an assistant's delivery, "" for any
// other or when nothing needs fixing. The warning that the delivery is over
// Limit names no size, so the block reads the same while it stays over.
func (d *delivery) warnings() string {
	if !d.assistant {
		return ""
	}
	var lines []string
	if d.over {
		lines = append(lines, fmt.Sprintf("Your memory is more than the %d bytes a hook's output reaches the model whole, so every session sees only its start until it reads the file the provider saved it to. Shorten %s and your context facts, and merge or delete facts.", Limit, agenthome.InstructionsFile))
	}
	if b := d.instructions; b != nil && b.out {
		lines = append(lines, fmt.Sprintf("%s (%d bytes) is more than the %d a delivery carries, so it is named instead of shown. Shorten it.", b.path, b.bytes, instructionsKeep))
	}
	problems := d.problems
	if len(problems) > maxListedProblems {
		problems = append(problems[:maxListedProblems:maxListedProblems], fmt.Sprintf("And %d more like these.", len(d.problems)-maxListedProblems))
	}
	lines = append(lines, problems...)
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

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
