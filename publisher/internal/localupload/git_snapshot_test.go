package localupload

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitSnapshotPublisherPushesSnapshotBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	directory := t.TempDir()
	remote := filepath.Join(directory, "remote.git")
	seed := filepath.Join(directory, "seed")
	repository := filepath.Join(directory, "repository")

	runGitTest(t, directory, "init", "--bare", remote)
	runGitTest(t, directory, "init", seed)
	if err := os.MkdirAll(filepath.Join(seed, "batches"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "batches", "existing.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, seed, "add", "--", "batches")
	runGitTest(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "seed")
	runGitTest(t, seed, "branch", "-M", "snapshot")
	runGitTest(t, seed, "remote", "add", "origin", remote)
	runGitTest(t, seed, "push", "-u", "origin", "snapshot")
	runGitTest(t, directory, "clone", "--branch", "snapshot", remote, repository)

	snapshotPath := filepath.Join(directory, "manual-snapshot.json")
	contents := "{\"version\":1,\"galleries\":[]}\n"
	if err := os.WriteFile(snapshotPath, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	publisher, err := NewGitSnapshotPublisher(repository)
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), snapshotPath); err != nil {
		t.Fatal(err)
	}
	got := runGitTest(t, directory, "--git-dir", remote, "show", "snapshot:batches/manual-snapshot.json")
	if strings.TrimSpace(got) != strings.TrimSpace(contents) {
		t.Fatalf("unexpected published snapshot: %q", got)
	}
}

func runGitTest(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}

func TestDisabledGitRepository(t *testing.T) {
	for _, value := range []string{"off", "OFF", " off ", "none", "-"} {
		if !DisabledGitRepository(value) {
			t.Fatalf("DisabledGitRepository(%q) = false, want true", value)
		}
	}
	for _, value := range []string{"", ".", "/home/user/xrw-album"} {
		if DisabledGitRepository(value) {
			t.Fatalf("DisabledGitRepository(%q) = true, want false", value)
		}
	}
}
