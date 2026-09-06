package main

import (
	"archive/zip"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/bundle"

	"gopkg.in/yaml.v3"
)

func TestPackageManagerPinsMatchAcrossBuildRoots(t *testing.T) {
	var want string
	for _, path := range []string{"frontend/package.json", "package.json", "mobile/package.json"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var pkg struct{ PackageManager string }
		if err := json.Unmarshal(body, &pkg); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(pkg.PackageManager, "pnpm@") {
			t.Fatalf("%s must pin pnpm before Corepack dispatches --dir", path)
		}
		if want == "" {
			want = pkg.PackageManager
		} else if pkg.PackageManager != want {
			t.Errorf("%s package manager differs from frontend/package.json", path)
		}
	}
}

// Framework symlinks are part of a signed app bundle. Following them while
// archiving changes the installed bundle even though every binary was signed.
func TestDarwinReleasePackagingPreservesFrameworkLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Darwin archives are built on Unix")
	}
	zipTool, err := exec.LookPath("zip")
	if err != nil {
		t.Skip("release packaging requires zip")
	}
	for _, source := range []string{"scripts/build-release.sh", ".github/workflows/release-build.yml"} {
		t.Run(source, func(t *testing.T) {
			body, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			command := regexp.MustCompile(`zip\s+(-[A-Za-z]+)\s+[^\n]*agent-overflow-darwin-arm64\.zip`).FindSubmatch(body)
			if len(command) != 2 {
				t.Fatal("Darwin zip invocation not found")
			}
			root := t.TempDir()
			bundle := "agent-overflow.app"
			versions := filepath.Join(root, bundle, "Contents", "Frameworks", "Example.framework", "Versions")
			if err := os.MkdirAll(filepath.Join(versions, "A"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(versions, "A", "Example"), []byte("signed framework"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("A", filepath.Join(versions, "Current")); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(t.TempDir(), "release.zip")
			cmd := exec.Command(zipTool, string(command[1]), archive, bundle)
			cmd.Dir = root
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("package: %v: %s", err, output)
			}
			reader, err := zip.OpenReader(archive)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			for _, file := range reader.File {
				if file.Name != filepath.ToSlash(filepath.Join(bundle, "Contents", "Frameworks", "Example.framework", "Versions", "Current")) {
					continue
				}
				if file.Mode()&os.ModeSymlink == 0 {
					t.Fatal("framework link became a regular file")
				}
				content, err := file.Open()
				if err != nil {
					t.Fatal(err)
				}
				link, err := io.ReadAll(content)
				content.Close()
				if err != nil || string(link) != "A" {
					t.Fatalf("framework link changed: %q (%v)", link, err)
				}
				return
			}
			t.Fatal("framework link disappeared from release archive")
		})
	}
}

func TestAndroidShellBuildMeetsBundleFloor(t *testing.T) {
	body, err := os.ReadFile("mobile/shell-build.txt")
	if err != nil {
		t.Fatal(err)
	}
	build, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil || build < 1 || build < bundle.MinShellBuild {
		t.Fatalf("Android shell build %q must be positive and >= bundle floor %d", strings.TrimSpace(string(body)), bundle.MinShellBuild)
	}
}

// A successful build is not a shipped artifact: the headless backend was
// built locally but omitted from CI's upload list. Follow every raw upload
// through the package job, and compare against the build script's outputs.
func TestReleaseWorkflowCarriesEveryArtifactToPackaging(t *testing.T) {
	body, err := os.ReadFile(".github/workflows/release-build.yml")
	if err != nil {
		t.Fatal(err)
	}
	type step struct {
		Uses string
		With map[string]string
	}
	var workflow struct {
		Jobs map[string]struct {
			Needs []string
			Steps []step
		}
	}
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatal(err)
	}
	packaging := workflow.Jobs["package"]
	consumed := map[string]bool{}
	for _, s := range packaging.Steps {
		if strings.HasPrefix(s.Uses, "actions/download-artifact@") {
			consumed[s.With["name"]] = true
		}
	}
	uploaded := map[string]bool{}
	for jobID, job := range workflow.Jobs {
		if jobID == "package" {
			continue
		}
		for _, s := range job.Steps {
			if !strings.HasPrefix(s.Uses, "actions/upload-artifact@") {
				continue
			}
			if !consumed[s.With["name"]] {
				t.Errorf("%s upload %q is not downloaded for packaging", jobID, s.With["name"])
			}
			if !strings.Contains(" "+strings.Join(packaging.Needs, " ")+" ", " "+jobID+" ") {
				t.Errorf("packaging does not depend on %s", jobID)
			}
			for line := range strings.SplitSeq(s.With["path"], "\n") {
				uploaded[filepath.Base(strings.TrimSpace(line))] = true
			}
		}
	}
	script, err := os.ReadFile("scripts/build-release.sh")
	if err != nil {
		t.Fatal(err)
	}
	names := regexp.MustCompile(`\$OUT_DIR/([A-Za-z0-9._-]+)`).FindAllStringSubmatch(string(script), -1)
	if len(names) == 0 {
		t.Fatal("no build outputs found")
	}
	for _, name := range names {
		if !uploaded[name[1]] {
			t.Errorf("built artifact %s is absent from CI uploads", name[1])
		}
	}
	if !uploaded["agent-overflow-android.apk"] {
		t.Error("Android APK is absent from CI uploads")
	}
}

// A tag promotes tested bytes. It must never trigger platform builds or
// regenerate checksums over an unverified replacement payload.
func TestReleaseTagOnlyPromotesSavedCandidate(t *testing.T) {
	body, err := os.ReadFile(".github/workflows/release-build.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			If          string
			Permissions map[string]string
			Steps       []struct {
				Name string
				Run  string
				Uses string
			}
		}
	}
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"linux-wsl", "macos", "android", "package"} {
		job := workflow.Jobs[name]
		if job.If != "github.event_name == 'workflow_dispatch'" {
			t.Errorf("%s must build only manual candidates", name)
		}
		if job.Permissions["contents"] == "write" {
			t.Errorf("candidate job %s must not publish", name)
		}
	}
	promote := workflow.Jobs["promote"]
	if promote.If != "github.event_name == 'push' && github.ref_type == 'tag'" {
		t.Fatal("promotion must run only for a pushed version tag")
	}
	verified := false
	published := false
	for _, step := range promote.Steps {
		if strings.Contains(step.Run, "scripts/release-candidate.mjs download") {
			verified = true
		}
		if strings.Contains(step.Run, "gh release create") {
			if !verified || !strings.Contains(step.Run, "--verify-tag") {
				t.Error("release must follow candidate verification and require an existing tag")
			}
			published = true
		}
		for _, forbidden := range []string{"build-release.sh", "package-release-assets.sh", "wails3", "build-apk.sh"} {
			if strings.Contains(step.Run, forbidden) {
				t.Errorf("promotion must not rebuild/repackage: %s", forbidden)
			}
		}
	}
	if !verified || !published {
		t.Fatal("promotion must verify saved artifacts then publish them")
	}
	// Hosted runner SDK installations do not guarantee sdkmanager is on PATH.
	androidSDK := false
	for _, step := range workflow.Jobs["android"].Steps {
		if strings.HasPrefix(step.Uses, "android-actions/setup-android@") {
			androidSDK = true
		}
		if strings.Contains(step.Run, "build-apk.sh") && !androidSDK {
			t.Error("Android release must provision its SDK before building")
		}
	}
	if !androidSDK {
		t.Error("Android release must explicitly provision its SDK")
	}
}
