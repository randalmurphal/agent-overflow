package starters

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
)

// File is one file in an embedded starter definition set.
type File struct {
	Name string
	Data []byte
}

// Set is one complete workflow definition and its prompt files.
type Set struct {
	Name  string
	Files []File
}

var names = []string{
	"build-and-validate",
	"converge-on-review",
	"multi-lens-review",
	"poll-jira-and-start",
	"port-campaign",
	"port-one-task",
}

//go:embed content/*/*
var content embed.FS

// List returns the available starter names in stable order.
func List() []string { return append([]string(nil), names...) }

// Fetch returns an isolated copy of one complete starter definition set.
func Fetch(name string) (Set, error) {
	if !known(name) {
		return Set{}, fmt.Errorf("unknown workflow starter %q", name)
	}
	entries, err := fs.ReadDir(content, "content/"+name)
	if err != nil {
		return Set{}, fmt.Errorf("read embedded workflow starter %q: %w", name, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	files := make([]File, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			return Set{}, fmt.Errorf("embedded workflow starter %q contains unexpected directory %q", name, entry.Name())
		}
		data, err := content.ReadFile("content/" + name + "/" + entry.Name())
		if err != nil {
			return Set{}, fmt.Errorf("read embedded workflow starter %q file %q: %w", name, entry.Name(), err)
		}
		files = append(files, File{Name: entry.Name(), Data: append([]byte(nil), data...)})
	}
	return Set{Name: name, Files: files}, nil
}

func known(name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}
