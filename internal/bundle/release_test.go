package bundle

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"testing/fstest"
)

func TestReleaseMetadataBindsVersionToHashedArchive(t *testing.T) {
	tree := fstest.MapFS{"index.html": {Data: []byte("<html>same UI</html>")}, ReleaseFileName: {Data: []byte(`{"version":"1.2.3-rc.4+build.5"}`)}}
	b := New(tree, "99.0.0")
	manifest, err := b.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Version != "1.2.3-rc.4+build.5" {
		t.Fatalf("version follows binary instead of UI: %q", manifest.Version)
	}
	archive, err := b.Archive()
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	file, err := zr.Open(ReleaseFileName)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, tree[ReleaseFileName].Data) {
		t.Fatal("archive omitted or changed hashed release metadata")
	}
	tree[ReleaseFileName] = &fstest.MapFile{Data: []byte(`{"version":"1.2.3"}`)}
	newer, err := New(tree, "99.0.0").Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if newer.ID == manifest.ID {
		t.Fatal("release version does not affect bundle identity")
	}
	if _, err := New(tree, "dev").Manifest(); err != nil {
		t.Fatal(err)
	}
}

func TestMalformedReleaseMetadataNeverFallsBackToBinaryVersion(t *testing.T) {
	cases := []string{"", `{}`, `null`, `[]`, `{"version":null}`, `{"version":123}`, `{"Version":"1.2.3"}`, `{"version":"1.2.3","other":true}`, `{"version":"1.2.3"} {}`, strings.Repeat(" ", 4097)}
	for _, version := range []string{"", "dev", "v1.2.3", "1.2", "01.2.3", "1.2.3-01", "1.2.3\n", "1.2.3-", "1.2.3+"} {
		data, _ := json.Marshal(map[string]string{"version": version})
		cases = append(cases, string(data))
	}
	for _, data := range cases {
		t.Run(data[:min(60, len(data))], func(t *testing.T) {
			tree := fstest.MapFS{"index.html": {Data: []byte("UI")}, ReleaseFileName: {Data: []byte(data)}}
			if _, err := New(tree, "99.0.0").Manifest(); err == nil {
				t.Fatal("invalid release metadata accepted")
			}
		})
	}
}
