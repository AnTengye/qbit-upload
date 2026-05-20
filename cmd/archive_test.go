package cmd

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestCreateTgzIncludesOnlyRequestedFiles(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.mp4"), "aaa")
	mustWriteFile(t, filepath.Join(root, "nested", "b.mkv"), "bbb")
	mustWriteFile(t, filepath.Join(root, "skip.txt"), "skip")

	out := filepath.Join(t.TempDir(), "out.tgz")
	if err := createTgzArchive(root, out, []string{"a.mp4", filepath.Join("nested", "b.mkv")}); err != nil {
		t.Fatalf("createTgzArchive returned error: %v", err)
	}

	got := readTgzNames(t, out)
	want := []string{"a.mp4", "nested/b.mkv"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tgz entries = %#v, want %#v", got, want)
	}
}

func TestDiscoverSevenZipPrefersEmbeddedCandidate(t *testing.T) {
	execDir := t.TempDir()
	embedded := filepath.Join(execDir, "tools", runtime.GOOS+"-"+runtime.GOARCH, preferredSevenZipNames()[0])
	mustWriteExecutable(t, embedded)

	got, err := discoverSevenZip("", execDir, "tools", func(name string) (string, error) {
		return filepath.Join(t.TempDir(), name), nil
	})
	if err != nil {
		t.Fatalf("discoverSevenZip returned error: %v", err)
	}
	if got != embedded {
		t.Fatalf("discoverSevenZip = %q, want %q", got, embedded)
	}
}

func TestDiscoverSevenZipPrefersOfficialLinuxCommandName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows official command remains 7z.exe")
	}
	execDir := t.TempDir()
	oldPort := filepath.Join(execDir, "tools", runtime.GOOS+"-"+runtime.GOARCH, "7z")
	official := filepath.Join(execDir, "tools", runtime.GOOS+"-"+runtime.GOARCH, "7zz")
	mustWriteExecutable(t, oldPort)
	mustWriteExecutable(t, official)

	got, err := discoverSevenZip("", execDir, "tools", nil)
	if err != nil {
		t.Fatalf("discoverSevenZip returned error: %v", err)
	}
	if got != official {
		t.Fatalf("discoverSevenZip = %q, want official 7zz %q", got, official)
	}
}

func TestDiscoverSevenZipUsesExplicitPath(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), executableName("7z"))
	mustWriteExecutable(t, explicit)

	got, err := discoverSevenZip(explicit, t.TempDir(), "tools", nil)
	if err != nil {
		t.Fatalf("discoverSevenZip returned error: %v", err)
	}
	if got != explicit {
		t.Fatalf("discoverSevenZip = %q, want explicit path", got)
	}
}

func TestDiscoverSevenZipLooksUpExplicitCommandName(t *testing.T) {
	want := filepath.Join(t.TempDir(), executableName("7z"))
	got, err := discoverSevenZip("7z", t.TempDir(), "tools", func(name string) (string, error) {
		if name != "7z" {
			t.Fatalf("lookPath called with %q, want 7z", name)
		}
		return want, nil
	})
	if err != nil {
		t.Fatalf("discoverSevenZip returned error: %v", err)
	}
	if got != want {
		t.Fatalf("discoverSevenZip = %q, want %q", got, want)
	}
}

func TestBuildSevenZipArgsEnablesProgressOutput(t *testing.T) {
	args := buildSevenZipArgs("out.7z", []string{"a.mp4"}, "secret", 64, 2)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-bsp1") {
		t.Fatalf("7z args %q missing -bsp1 progress output switch", joined)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func mustWriteExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("fake"), 0o755); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func readTgzNames(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("NewReader(%s): %v", path, err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar Next: %v", err)
		}
		names = append(names, hdr.Name)
	}
	sort.Strings(names)
	return names
}
