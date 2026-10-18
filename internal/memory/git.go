package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// DefaultGitTimeout bounds each git command Dream runs.
const DefaultGitTimeout = 30 * time.Second

// dreamGitignore is written when Dream creates the workspace repository,
// like nanobot's GitStore: only long-term memory is versioned, never
// history.jsonl, the cursor or the rest of the workspace.
const dreamGitignore = `# Written by nanobot dream: only long-term memory is versioned.
/*
!/.gitignore
!/SOUL.md
!/USER.md
!/memory/
/memory/*
!/memory/MEMORY.md
!/skills/
`

// gitRepoEnv lists environment variables that would point git at another
// repository, index or work tree than the workspace.
var gitRepoEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_NAMESPACE", "GIT_PREFIX",
}

// gitStore commits Dream's edits to a git repository at the workspace
// root, using the git binary.
type gitStore struct {
	bin       string // as configured; "" means "git"
	workspace string
	timeout   time.Duration
	log       *slog.Logger
}

// errNoGit marks a missing git binary, which disables commits.
var errNoGit = errors.New("git not found")

// commit stages paths (workspace-relative, slash-separated; missing ones
// are skipped unless git tracks them) and commits them with message. It
// returns the short hash, or "" when nothing changed. Only those paths are
// committed, never other changes the user has staged.
//
// When git is not installed, or the workspace sits inside another
// repository (which Dream does not commit to), commit logs why and
// returns "" with no error. A workspace that is not in any repository is
// initialized with a .gitignore that versions only long-term memory.
func (g *gitStore) commit(ctx context.Context, paths []string, message string) (string, error) {
	bin, err := g.lookPath()
	if err != nil {
		g.log.Info("dream: git is not installed; memory changes are not committed", "err", err)
		return "", nil
	}
	ok, created, err := g.ensureRepo(ctx, bin)
	if err != nil || !ok {
		return "", err
	}
	if created {
		paths = append(paths, ".gitignore")
	}

	tracked, err := g.run(ctx, bin, append([]string{"ls-files", "-z", "--"}, paths...)...)
	if err != nil {
		return "", err
	}
	var stage []string
	for _, p := range paths {
		if _, err := os.Lstat(filepath.Join(g.workspace, filepath.FromSlash(p))); err == nil || isTracked(tracked, p) {
			stage = append(stage, p)
		}
	}
	if len(stage) == 0 {
		return "", nil
	}
	if _, err := g.run(ctx, bin, append([]string{"add", "-A", "--"}, stage...)...); err != nil {
		return "", err
	}
	changes, err := g.run(ctx, bin, append([]string{"diff", "--cached", "--name-status", "--"}, stage...)...)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(changes) == "" {
		return "", nil
	}
	message += "\n\n" + strings.TrimSpace(changes) + "\n"
	// A fixed identity (like nanobot's "nanobot <nanobot@dream>") marks
	// the commit as the bot's, and works where none is configured. Hooks
	// and signing are skipped: this runs unattended.
	args := []string{
		"-c", "user.name=nanobot", "-c", "user.email=nanobot@dream", "-c", "commit.gpgSign=false",
		"commit", "--quiet", "--no-verify", "-m", message, "--",
	}
	if _, err := g.run(ctx, bin, append(args, stage...)...); err != nil {
		return "", err
	}
	sha, err := g.run(ctx, bin, "rev-parse", "--short", "HEAD")
	return strings.TrimSpace(sha), err
}

func (g *gitStore) lookPath() (string, error) {
	name := g.bin
	if name == "" {
		name = "git"
	}
	bin, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errNoGit, err)
	}
	return bin, nil
}

// ensureRepo reports whether the workspace root is a repository Dream may
// commit to, initializing one if the workspace is in none. created is
// true when it wrote a new .gitignore.
func (g *gitStore) ensureRepo(ctx context.Context, bin string) (ok, created bool, err error) {
	// Like nanobot's GitStore, look for a .git entry (a directory, or a
	// file for worktrees and submodules) at the workspace and above, rather
	// than asking git: a git failure must not be mistaken for "no
	// repository" and lead to a nested one.
	dir, err := filepath.EvalSymlinks(g.workspace)
	if err != nil {
		return false, false, fmt.Errorf("git: %w", err)
	}
	for cur := dir; ; {
		if _, err := os.Lstat(filepath.Join(cur, ".git")); err == nil {
			if cur == dir {
				return true, false, nil
			}
			g.log.Warn("dream: workspace is inside another git repository; memory changes are not committed",
				"workspace", g.workspace, "repository", cur)
			return false, false, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	if _, err := g.run(ctx, bin, "init", "--quiet"); err != nil {
		return false, false, err
	}
	gitignore := filepath.Join(g.workspace, ".gitignore")
	f, err := os.OpenFile(gitignore, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return true, false, nil // the user's own rules win
	}
	if err != nil {
		return false, false, fmt.Errorf("git: write .gitignore: %w", err)
	}
	_, werr := f.WriteString(dreamGitignore)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return false, false, fmt.Errorf("git: write .gitignore: %w", werr)
	}
	g.log.Info("dream: initialized a git repository for memory changes", "workspace", g.workspace)
	return true, true, nil
}

// run runs git in the workspace with a timeout and returns its stdout.
func (g *gitStore) run(ctx context.Context, bin string, args ...string) (string, error) {
	timeout := g.timeout
	if timeout <= 0 {
		timeout = DefaultGitTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, append([]string{"-C", g.workspace}, args...)...)
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(gitRepoEnv, name)
	}), "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = time.Second // don't wait on pipes a killed git's children hold
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("%w (%w)", ctx.Err(), err)
		}
		sub := args[0]
		for i, a := range args {
			if a == "-c" || i > 0 && args[i-1] == "-c" {
				continue
			}
			sub = a
			break
		}
		return "", fmt.Errorf("git %s: %w: %s", sub, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// isTracked reports whether the NUL-separated ls-files output lists p or a
// file below it.
func isTracked(lsFiles, p string) bool {
	for name := range strings.SplitSeq(lsFiles, "\x00") {
		if name == p || strings.HasPrefix(name, p+"/") {
			return true
		}
	}
	return false
}
