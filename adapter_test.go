package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestGitBranch covers the cases adapter.go's gitBranch has to recognise:
// not-a-repo, regular .git directory, linked worktree (relative gitdir),
// linked worktree (absolute gitdir), detached HEAD, and an empty cwd. The
// linked-worktree case is the rt6 bug: before the fix, reading <cwd>/.git/HEAD
// returned the .git pointer file's contents and the branch came out as either
// "gitdir: ..." or a detached SHA, depending on whether the CutPrefix matched.
func TestGitBranch(t *testing.T) {
	tmp := t.TempDir()

	// Empty cwd -> no work to do, no branch.
	if got := gitBranch(""); got != "" {
		t.Errorf("gitBranch(\"\") = %q, want \"\"", got)
	}

	// Not a repo at all.
	noRepo := filepath.Join(tmp, "no-repo")
	if err := os.Mkdir(noRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := gitBranch(noRepo); got != "" {
		t.Errorf("gitBranch(no repo) = %q, want \"\"", got)
	}

	// A regular repo: .git is a directory, HEAD points at refs/heads/main.
	mainRepo := filepath.Join(tmp, "main")
	mainGit := filepath.Join(mainRepo, ".git")
	if err := os.MkdirAll(mainGit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mainGit, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := gitBranch(mainRepo); got != "main" {
		t.Errorf("gitBranch(main repo) = %q, want \"main\"", got)
	}

	// Linked worktree with a *relative* gitdir. This is the form git
	// actually writes by default, e.g. "../main/.git/worktrees/wt". The
	// path is anchored against the .git file's parent (the worktree's cwd).
	wtGitdir := filepath.Join(mainGit, "worktrees", "wt")
	if err := os.MkdirAll(wtGitdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtGitdir, "HEAD"), []byte("ref: refs/heads/feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wtRel := filepath.Join(tmp, "wt-rel")
	if err := os.Mkdir(wtRel, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtRel, ".git"), []byte("gitdir: ../main/.git/worktrees/wt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := gitBranch(wtRel); got != "feature" {
		t.Errorf("gitBranch(worktree, relative) = %q, want \"feature\"", got)
	}

	// Linked worktree with an *absolute* gitdir. Same outcome, exercises
	// the filepath.IsAbs branch in worktreeGitDir.
	wtAbs := filepath.Join(tmp, "wt-abs")
	if err := os.Mkdir(wtAbs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtAbs, ".git"), []byte("gitdir: "+wtGitdir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := gitBranch(wtAbs); got != "feature" {
		t.Errorf("gitBranch(worktree, absolute) = %q, want \"feature\"", got)
	}

	// Detached HEAD: HEAD is a raw SHA, not a ref. We fall through to the
	// SHA so the user can still tell which commit they're on.
	detached := filepath.Join(tmp, "detached")
	if err := os.MkdirAll(filepath.Join(detached, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(detached, ".git", "HEAD"), []byte("abc1234567890abcdef1234567890abcdef12345\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := gitBranch(detached); got != "abc1234567890abcdef1234567890abcdef12345" {
		t.Errorf("gitBranch(detached) = %q, want SHA", got)
	}

	// .git file that doesn't start with "gitdir: " — oddball, but we should
	// not panic or fabricate a branch.
	bogus := filepath.Join(tmp, "bogus")
	if err := os.Mkdir(bogus, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bogus, ".git"), []byte("not a gitdir line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := gitBranch(bogus); got != "" {
		t.Errorf("gitBranch(bogus .git file) = %q, want \"\"", got)
	}
}
