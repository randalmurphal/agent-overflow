// Package skills embeds the vendored skills tree the Settings page installs
// into the provider homes (docs/specs/standing-agents.md, "Skills library").
// Only the skill directories and the upstream pin are embedded: Install
// copies `pstack/skills/<name>`, and the version shown comes from UPSTREAM.md.
// A directory pattern leaves out names starting with `.` or `_`, so the bun
// dependency links `make pstack-scripts` leaves under node_modules/.bin never
// reach the embed; internal/skillsinstall skips node_modules itself.
package skills

import (
	"embed"
	"io/fs"
)

//go:embed pstack/skills pstack/UPSTREAM.md
var files embed.FS

// PStackSkills is the tree whose top-level directories are the skills.
func PStackSkills() fs.FS {
	sub, err := fs.Sub(files, "pstack/skills")
	if err != nil {
		panic(err) // the pattern above embeds this directory
	}
	return sub
}

// PStackUpstream is UPSTREAM.md, which pins the upstream commit and version.
func PStackUpstream() []byte {
	data, err := files.ReadFile("pstack/UPSTREAM.md")
	if err != nil {
		panic(err) // the pattern above embeds this file
	}
	return data
}
