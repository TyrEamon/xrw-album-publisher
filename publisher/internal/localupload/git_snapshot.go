package localupload

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

type GitSnapshotPublisher struct {
	repository string
	mu         sync.Mutex
}

func NewGitSnapshotPublisher(repository string) (*GitSnapshotPublisher, error) {
	repository, err := filepath.Abs(strings.TrimSpace(repository))
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(repository, ".git")); err != nil {
		return nil, fmt.Errorf("Git repository not found at %s", repository)
	}
	return &GitSnapshotPublisher{repository: repository}, nil
}

// DisabledGitRepository reports whether the configured repository asks for
// local-only snapshots. The phone build ships no git checkout, and a missing
// .git would otherwise abort startup.
func DisabledGitRepository(repository string) bool {
	switch strings.ToLower(strings.TrimSpace(repository)) {
	case "off", "none", "-":
		return true
	default:
		return false
	}
}

func (p *GitSnapshotPublisher) Publish(ctx context.Context, snapshotPath string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	name := filepath.Base(snapshotPath)
	if filepath.Ext(name) != ".json" {
		return errors.New("snapshot file must be JSON")
	}
	gitPath := filepath.ToSlash(filepath.Join("batches", name))
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := p.git(ctx, nil, "fetch", "origin", "snapshot"); err != nil {
			return err
		}
		parent, err := p.git(ctx, nil, "rev-parse", "origin/snapshot")
		if err != nil {
			return err
		}
		commit, changed, err := p.makeCommit(ctx, snapshotPath, gitPath, parent)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}
		if _, err := p.git(ctx, nil, "push", "origin", commit+":snapshot"); err == nil {
			return nil
		} else if attempt == 3 {
			return err
		}
	}
	return nil
}

func (p *GitSnapshotPublisher) makeCommit(ctx context.Context, snapshotPath, gitPath, parent string) (string, bool, error) {
	index, err := os.CreateTemp(filepath.Dir(snapshotPath), "snapshot-git-index-*")
	if err != nil {
		return "", false, err
	}
	indexPath := index.Name()
	if err := index.Close(); err != nil {
		return "", false, err
	}
	if err := os.Remove(indexPath); err != nil {
		return "", false, err
	}
	defer os.Remove(indexPath)

	gitEnvironment := append(os.Environ(), "GIT_INDEX_FILE="+indexPath)
	if _, err := p.git(ctx, gitEnvironment, "read-tree", parent); err != nil {
		return "", false, err
	}
	blob, err := p.git(ctx, nil, "hash-object", "--no-filters", "-w", "--", snapshotPath)
	if err != nil {
		return "", false, err
	}
	if _, err := p.git(ctx, gitEnvironment, "update-index", "--add", "--cacheinfo", "100644", blob, gitPath); err != nil {
		return "", false, err
	}
	tree, err := p.git(ctx, gitEnvironment, "write-tree")
	if err != nil {
		return "", false, err
	}
	parentTree, err := p.git(ctx, nil, "rev-parse", parent+"^{tree}")
	if err != nil {
		return "", false, err
	}
	if tree == parentTree {
		return parent, false, nil
	}

	identity := append(gitEnvironment,
		"GIT_AUTHOR_NAME=xrw-local-uploader",
		"GIT_AUTHOR_EMAIL=xrw-local-uploader@users.noreply.github.com",
		"GIT_COMMITTER_NAME=xrw-local-uploader",
		"GIT_COMMITTER_EMAIL=xrw-local-uploader@users.noreply.github.com",
	)
	message := "Add local gallery " + strings.TrimSuffix(filepath.Base(snapshotPath), ".json")
	commit, err := p.git(ctx, identity, "commit-tree", tree, "-p", parent, "-m", message)
	return commit, true, err
}

func (p *GitSnapshotPublisher) git(ctx context.Context, environment []string, arguments ...string) (string, error) {
	args := append([]string{"-C", p.repository}, arguments...)
	command := exec.CommandContext(ctx, "git", args...)
	if environment != nil {
		command.Env = environment
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}
