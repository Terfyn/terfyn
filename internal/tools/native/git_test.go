package native

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitCfg runs git in dir with a fixed identity and no signing, failing the test on error.
func gitCfg(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{
		"-c", "user.email=t@example.com", "-c", "user.name=Test",
		"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// initRepoWithCommit makes a git repo in a fresh temp dir with one commit and returns its path.
func initRepoWithCommit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitCfg(t, dir, "init")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCfg(t, dir, "add", "-A")
	gitCfg(t, dir, "commit", "-m", "seed")
	return dir
}

func TestValidateBranchName(t *testing.T) {
	ok := []string{"feature/x", "fix-123", "agentic/review", "a.b_c"}
	for _, n := range ok {
		if _, err := validateBranchName("name", n); err != nil {
			t.Errorf("%q should be valid: %v", n, err)
		}
	}
	bad := []string{"", "  ", "-force", "a b", "a:b", "a..b", "a~b", "a^b", "a?b", "a*b", "a\\b", "feature/", "/lead", "x.lock", "a@{b"}
	for _, n := range bad {
		if _, err := validateBranchName("name", n); err == nil {
			t.Errorf("%q should be rejected", n)
		}
	}
}

func TestGitCreateBranch(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)

	out, _, err := NewRegistry().Dispatch(context.Background(), "create_branch", map[string]any{"name": "feature/x"})
	if err != nil {
		t.Fatalf("create_branch: %v", err)
	}
	if out["branch"] != "feature/x" || out["created"] != true {
		t.Fatalf("result %#v", out)
	}
	if cur := gitCfg(t, root, "rev-parse", "--abbrev-ref", "HEAD"); cur != "feature/x" {
		t.Fatalf("current branch = %q, want feature/x", cur)
	}
}

// Re-running create_branch for an existing branch is idempotent: it switches to the branch and
// reports created=false, instead of failing with "already exists" (issue #517).
func TestGitCreateBranch_idempotentWhenExists(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	reg := NewRegistry()

	if _, _, err := reg.Dispatch(context.Background(), "create_branch", map[string]any{"name": "fix/264"}); err != nil {
		t.Fatalf("first create_branch: %v", err)
	}
	// Switch away, then re-run: the branch already exists.
	gitCfg(t, root, "switch", "main")
	out, _, err := reg.Dispatch(context.Background(), "create_branch", map[string]any{"name": "fix/264"})
	if err != nil {
		t.Fatalf("re-run create_branch must not fail on an existing branch: %v", err)
	}
	if out["branch"] != "fix/264" || out["created"] != false {
		t.Fatalf("result %#v, want created=false", out)
	}
	if cur := gitCfg(t, root, "rev-parse", "--abbrev-ref", "HEAD"); cur != "fix/264" {
		t.Fatalf("current branch = %q, want fix/264 (should have switched to it)", cur)
	}
}

// reset:true with base force-recreates the branch at the start point, discarding a prior attempt's
// commit — including the #517 re-run topology where HEAD is STILL on the fix branch (no switch away).
func TestGitCreateBranch_resetDiscardsPriorWork(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	reg := NewRegistry()
	mainSHA := gitCfg(t, root, "rev-parse", "HEAD")

	if _, _, err := reg.Dispatch(context.Background(), "create_branch", map[string]any{"name": "fix/264"}); err != nil {
		t.Fatal(err)
	}
	// A prior attempt commits onto the branch, and the workspace is LEFT on the fix branch (the state
	// a re-run of the workflow actually sees — no switch back to main).
	if err := os.WriteFile(filepath.Join(root, "attempt.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCfg(t, root, "add", "-A")
	gitCfg(t, root, "commit", "-m", "half-finished attempt")
	if gitCfg(t, root, "rev-parse", "HEAD") == mainSHA {
		t.Fatal("precondition: branch should have advanced past main")
	}
	if cur := gitCfg(t, root, "rev-parse", "--abbrev-ref", "HEAD"); cur != "fix/264" {
		t.Fatalf("precondition: HEAD should still be on fix/264, got %q", cur)
	}

	out, _, err := reg.Dispatch(context.Background(), "create_branch", map[string]any{"name": "fix/264", "reset": true, "base": "main"})
	if err != nil {
		t.Fatalf("reset create_branch: %v", err)
	}
	if out["reset"] != true || out["created"] != false {
		t.Fatalf("result %#v", out)
	}
	// The branch is back at main — the prior attempt's commit is gone even though HEAD was on it.
	if head := gitCfg(t, root, "rev-parse", "HEAD"); head != mainSHA {
		t.Fatalf("HEAD = %q, want reset back to main %q", head, mainSHA)
	}
	if _, err := os.Stat(filepath.Join(root, "attempt.txt")); !os.IsNotExist(err) {
		t.Fatalf("attempt.txt should be gone after reset, err=%v", err)
	}
}

// reset:true without base is refused (a reset to the current HEAD would be a no-op that silently
// keeps the prior attempt's commits) — fail closed rather than claim a reset that did nothing (#517).
func TestGitCreateBranch_resetRequiresBase(t *testing.T) {
	requireGit(t)
	t.Setenv(envWorkspaceRoot, initRepoWithCommit(t))
	_, _, err := NewRegistry().Dispatch(context.Background(), "create_branch", map[string]any{"name": "fix/264", "reset": true})
	if err == nil {
		t.Fatal("reset without base must be refused")
	}
	if !strings.Contains(err.Error(), "reset requires base") {
		t.Fatalf("error should explain reset needs a base, got: %v", err)
	}
}

// base creates the branch from a given start point rather than the current HEAD.
func TestGitCreateBranch_baseStartPoint(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	reg := NewRegistry()
	mainSHA := gitCfg(t, root, "rev-parse", "HEAD")

	// Advance a "dev" branch past main, then create fix off main while HEAD is on dev.
	gitCfg(t, root, "switch", "-c", "dev")
	if err := os.WriteFile(filepath.Join(root, "dev.txt"), []byte("dev\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCfg(t, root, "add", "-A")
	gitCfg(t, root, "commit", "-m", "dev work")

	out, _, err := reg.Dispatch(context.Background(), "create_branch", map[string]any{"name": "fix/off-main", "base": "main"})
	if err != nil {
		t.Fatalf("create_branch with base: %v", err)
	}
	if out["created"] != true {
		t.Fatalf("result %#v", out)
	}
	if head := gitCfg(t, root, "rev-parse", "HEAD"); head != mainSHA {
		t.Fatalf("branch created off HEAD %q, want off base main %q", head, mainSHA)
	}
}

func TestGitCreateBranch_rejectsFlagName(t *testing.T) {
	requireGit(t)
	t.Setenv(envWorkspaceRoot, initRepoWithCommit(t))
	if _, _, err := NewRegistry().Dispatch(context.Background(), "create_branch", map[string]any{"name": "--force"}); err == nil {
		t.Fatal("a branch name that looks like a flag must be rejected")
	}
}

func TestGitPushBranch(t *testing.T) {
	requireGit(t)
	remote := t.TempDir()
	gitCfg(t, remote, "init", "--bare")

	root := initRepoWithCommit(t)
	gitCfg(t, root, "remote", "add", "origin", remote)
	gitCfg(t, root, "switch", "-c", "feature/y")
	t.Setenv(envWorkspaceRoot, root)

	out, _, err := NewRegistry().Dispatch(context.Background(), "push_branch", map[string]any{"branch": "feature/y"})
	if err != nil {
		t.Fatalf("push_branch: %v", err)
	}
	if out["pushed"] != true || out["remote"] != "origin" {
		t.Fatalf("result %#v", out)
	}
	// The bare remote now has the branch ref.
	if _, err := exec.Command("git", "--git-dir="+remote, "rev-parse", "refs/heads/feature/y").Output(); err != nil {
		t.Fatalf("remote should have refs/heads/feature/y: %v", err)
	}
}

func TestGitPushBranch_customRemote(t *testing.T) {
	requireGit(t)
	remote := t.TempDir()
	gitCfg(t, remote, "init", "--bare")
	root := initRepoWithCommit(t)
	gitCfg(t, root, "remote", "add", "upstream", remote)
	gitCfg(t, root, "switch", "-c", "feature/z")
	t.Setenv(envWorkspaceRoot, root)
	t.Setenv(envGitRemote, "upstream")

	if _, _, err := NewRegistry().Dispatch(context.Background(), "push_branch", map[string]any{"branch": "feature/z"}); err != nil {
		t.Fatalf("push_branch to custom remote: %v", err)
	}
	if _, err := exec.Command("git", "--git-dir="+remote, "rev-parse", "refs/heads/feature/z").Output(); err != nil {
		t.Fatalf("upstream should have refs/heads/feature/z: %v", err)
	}
}

// TestGitPushBranch_refusesDefaultBranch is the invariant the tool exists to hold: a fix must land
// on a review branch, never on the remote's default. The bare remote's default is main (init
// default), so pushing main is refused and the ref does not move.
func TestGitPushBranch_refusesDefaultBranch(t *testing.T) {
	requireGit(t)
	remote := t.TempDir()
	gitCfg(t, remote, "init", "--bare")

	root := initRepoWithCommit(t) // on default branch (main)
	gitCfg(t, root, "remote", "add", "origin", remote)
	// Seed the remote's default so ls-remote --symref resolves it, then advance locally.
	gitCfg(t, root, "push", "origin", "refs/heads/main:refs/heads/main")
	before := gitCfg(t, remote, "rev-parse", "refs/heads/main")
	if err := os.WriteFile(filepath.Join(root, "x.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCfg(t, root, "add", "-A")
	gitCfg(t, root, "commit", "-m", "local advance")
	t.Setenv(envWorkspaceRoot, root)

	_, _, err := NewRegistry().Dispatch(context.Background(), "push_branch", map[string]any{"branch": "main"})
	if err == nil || !strings.Contains(err.Error(), "default branch") {
		t.Fatalf("pushing the default branch must be refused, got %v", err)
	}
	if after := gitCfg(t, remote, "rev-parse", "refs/heads/main"); after != before {
		t.Fatalf("remote default branch moved despite the refusal: %s -> %s", before, after)
	}
}

// TestGitCommit is the core of the #528 fix: an agent's edit lives only in the working tree, and
// commit materializes it onto the branch so a later push has something to push. It reports the new
// HEAD sha and committed=true.
func TestGitCommit(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	// The commit uses the ambient git identity; configure it locally so the op does not depend on a
	// developer's global gitconfig.
	gitCfg(t, root, "config", "user.email", "t@example.com")
	gitCfg(t, root, "config", "user.name", "Test")
	gitCfg(t, root, "config", "commit.gpgsign", "false")
	t.Setenv(envWorkspaceRoot, root)
	before := gitCfg(t, root, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(root, "fix.txt"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, err := NewRegistry().Dispatch(context.Background(), "commit", map[string]any{"message": "apply the fix"})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if out["committed"] != true {
		t.Fatalf("result %#v, want committed=true", out)
	}
	after := gitCfg(t, root, "rev-parse", "HEAD")
	if after == before {
		t.Fatal("HEAD did not advance after commit")
	}
	if out["sha"] != after {
		t.Fatalf("result sha = %v, want HEAD %q", out["sha"], after)
	}
	// A clean tree afterwards proves the change was staged and committed, not left behind.
	if st := gitCfg(t, root, "status", "--porcelain"); st != "" {
		t.Fatalf("working tree not clean after commit: %q", st)
	}
	if msg := gitCfg(t, root, "log", "-1", "--pretty=%s"); msg != "apply the fix" {
		t.Fatalf("commit subject = %q, want %q", msg, "apply the fix")
	}
}

// A no-op change is a graceful outcome, not a failure: commit reports committed=false with a reason
// and leaves HEAD where it was, so a run does not crash when the deliverable already existed (#528).
func TestGitCommit_nothingToCommit(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	before := gitCfg(t, root, "rev-parse", "HEAD")

	out, _, err := NewRegistry().Dispatch(context.Background(), "commit", map[string]any{"message": "nothing here"})
	if err != nil {
		t.Fatalf("commit with no changes must not fail: %v", err)
	}
	if out["committed"] != false || out["reason"] != "nothing to commit" {
		t.Fatalf("result %#v, want committed=false reason=nothing to commit", out)
	}
	if after := gitCfg(t, root, "rev-parse", "HEAD"); after != before {
		t.Fatalf("HEAD moved on a no-op commit: %s -> %s", before, after)
	}
}

// An explicit paths list stages only those files; other working-tree changes are left uncommitted.
func TestGitCommit_explicitPaths(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	gitCfg(t, root, "config", "user.email", "t@example.com")
	gitCfg(t, root, "config", "user.name", "Test")
	gitCfg(t, root, "config", "commit.gpgsign", "false")
	t.Setenv(envWorkspaceRoot, root)

	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "later.txt"), []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, err := NewRegistry().Dispatch(context.Background(), "commit", map[string]any{
		"message": "only keep.txt",
		"paths":   []any{"keep.txt"},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if out["committed"] != true {
		t.Fatalf("result %#v", out)
	}
	// keep.txt is committed; later.txt remains an untracked working-tree change.
	if st := gitCfg(t, root, "status", "--porcelain"); st != "?? later.txt" {
		t.Fatalf("status = %q, want only later.txt left untracked", st)
	}
}

// commit requires a message.
func TestGitCommit_missingMessage(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	if err := os.WriteFile(filepath.Join(root, "x.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewRegistry().Dispatch(context.Background(), "commit", map[string]any{}); err == nil {
		t.Fatal("commit without a message must be rejected")
	}
}

// TestGitCommit_thenPush is the whole #528 happy path: edit -> commit -> push lands a branch that is
// ahead of its base, which is exactly the state pull_request.create needs (the missing commit was
// why it returned 422 "No commits").
func TestGitCommit_thenPush(t *testing.T) {
	requireGit(t)
	remote := t.TempDir()
	gitCfg(t, remote, "init", "--bare")

	root := initRepoWithCommit(t)
	gitCfg(t, root, "config", "user.email", "t@example.com")
	gitCfg(t, root, "config", "user.name", "Test")
	gitCfg(t, root, "config", "commit.gpgsign", "false")
	gitCfg(t, root, "remote", "add", "origin", remote)
	gitCfg(t, root, "switch", "-c", "fix/528")
	t.Setenv(envWorkspaceRoot, root)
	reg := NewRegistry()

	if err := os.WriteFile(filepath.Join(root, "fix.txt"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.Dispatch(context.Background(), "commit", map[string]any{"message": "fix #528"}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, _, err := reg.Dispatch(context.Background(), "push_branch", map[string]any{"branch": "fix/528"}); err != nil {
		t.Fatalf("push_branch: %v", err)
	}
	// The pushed branch carries the commit — the fix.txt blob is reachable from the remote ref.
	if out, err := exec.Command("git", "--git-dir="+remote, "log", "-1", "--pretty=%s", "refs/heads/fix/528").Output(); err != nil {
		t.Fatalf("remote should have the fix/528 commit: %v", err)
	} else if got := strings.TrimSpace(string(out)); got != "fix #528" {
		t.Fatalf("remote HEAD subject = %q, want %q", got, "fix #528")
	}
}

func TestGit_missingRoot(t *testing.T) {
	t.Setenv(envWorkspaceRoot, "")
	if _, _, err := NewRegistry().Dispatch(context.Background(), "create_branch", map[string]any{"name": "x"}); err == nil {
		t.Fatal("expected an error when the workspace root is unset")
	}
}
