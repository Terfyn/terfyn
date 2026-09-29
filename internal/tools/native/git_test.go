package native

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// gitCfg runs git in dir with a fixed identity and no signing, failing the test on error.
func gitCfg(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{
		"-c", "user.email=t@example.com", "-c", "user.name=Test",
		"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main",
		"-c", "core.autocrlf=false", "-c", "core.eol=lf",
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

func gitFilenameLegalOnOS(name string) bool {
	if runtime.GOOS != "windows" {
		return true
	}
	// Windows rejects <>:"/\|?* and control characters (tab/newline).
	if strings.ContainsAny(name, `<>:"/\|?*`) {
		return false
	}
	return !strings.ContainsAny(name, "\t\n\r")
}

// initRepoWithCommit makes a git repo in a fresh temp dir with one commit and returns its path.
func initRepoWithCommit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitCfg(t, dir, "init")
	// Pin LF so git reset --hard restores the seed blob as written. Windows CI has
	// core.autocrlf=true by default, which would checkout "seed\n" as "seed\r\n".
	gitCfg(t, dir, "config", "core.autocrlf", "false")
	gitCfg(t, dir, "config", "core.eol", "lf")
	// Some CI images lower core.bigFileThreshold (or treat a 1 MiB single line as
	// binary). Keep throwaway repos in the text-diff path so cap tests see real stdout.
	gitCfg(t, dir, "config", "core.bigFileThreshold", "2g")
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

// reset:true must not abort on leftover uncommitted edits (issue #531). A prior run that edited
// files and died before commit leaves a dirty tree; `switch -C` then fails with "local changes
// would be overwritten by checkout" unless the working tree is cleaned first.
func TestGitCreateBranch_resetCleansDirtyWorkingTree(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	reg := NewRegistry()
	mainSHA := gitCfg(t, root, "rev-parse", "HEAD")

	if _, _, err := reg.Dispatch(context.Background(), "create_branch", map[string]any{"name": "fix/264"}); err != nil {
		t.Fatal(err)
	}
	// Commit one change so the fix branch diverges from main (otherwise checkout has nothing to
	// overwrite and git will keep the dirt). Then leave further uncommitted edits, matching a
	// failed run that never reached git.commit.
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("committed-wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCfg(t, root, "add", "-A")
	gitCfg(t, root, "commit", "-m", "half-finished attempt")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("uncommitted-wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "leftover.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _, err := reg.Dispatch(context.Background(), "create_branch", map[string]any{"name": "fix/264", "reset": true, "base": "main"})
	if err != nil {
		t.Fatalf("reset create_branch with a dirty tree: %v", err)
	}
	if out["reset"] != true {
		t.Fatalf("result %#v", out)
	}
	if head := gitCfg(t, root, "rev-parse", "HEAD"); head != mainSHA {
		t.Fatalf("HEAD = %q, want reset back to main %q", head, mainSHA)
	}
	got, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "seed\n" {
		t.Fatalf("README.md = %q, want seed content after reset", got)
	}
	if _, err := os.Stat(filepath.Join(root, "leftover.txt")); !os.IsNotExist(err) {
		t.Fatalf("leftover.txt should be gone after reset, err=%v", err)
	}
}

// Without reset, a dirty tree that would be overwritten still fails — reset is the opt-in
// destructive recovery path; the default create/switch must not clobber in-progress edits.
func TestGitCreateBranch_dirtyTreeStillBlocksWithoutReset(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	reg := NewRegistry()

	if _, _, err := reg.Dispatch(context.Background(), "create_branch", map[string]any{"name": "fix/264"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("on-fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCfg(t, root, "add", "-A")
	gitCfg(t, root, "commit", "-m", "fix-branch work")
	gitCfg(t, root, "switch", "main")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("dirty-on-main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := reg.Dispatch(context.Background(), "create_branch", map[string]any{"name": "fix/264"})
	if err == nil {
		t.Fatal("create_branch without reset must refuse to overwrite a dirty tree")
	}
	if !strings.Contains(err.Error(), "overwritten") && !strings.Contains(err.Error(), "local changes") {
		t.Fatalf("error should mention the dirty tree, got: %v", err)
	}
}

// An invalid base must fail before any destructive cleanup so leftover work is still on disk.
func TestGitCreateBranch_resetInvalidBasePreservesWork(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	reg := NewRegistry()

	if _, _, err := reg.Dispatch(context.Background(), "create_branch", map[string]any{"name": "fix/264"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("uncommitted-wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "leftover.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := reg.Dispatch(context.Background(), "create_branch", map[string]any{"name": "fix/264", "reset": true, "base": "maim"})
	if err == nil {
		t.Fatal("reset with a bogus base must fail")
	}
	if !strings.Contains(err.Error(), `base "maim" is not a commit`) {
		t.Fatalf("error should name the invalid base, got: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "uncommitted-wip\n" {
		t.Fatalf("README.md = %q, want uncommitted work preserved", got)
	}
	if _, err := os.Stat(filepath.Join(root, "leftover.txt")); err != nil {
		t.Fatalf("leftover.txt should still exist after a failed reset, err=%v", err)
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

// An explicit paths list scopes the commit itself, not just staging: an unrelated change already
// staged in the index is not swept into the commit (it still commits only the requested paths).
func TestGitCommit_explicitPathsDoNotSweepPreStaged(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	gitCfg(t, root, "config", "user.email", "t@example.com")
	gitCfg(t, root, "config", "user.name", "Test")
	gitCfg(t, root, "config", "commit.gpgsign", "false")
	t.Setenv(envWorkspaceRoot, root)

	// An unrelated change is staged before the op runs, plus the requested change.
	if err := os.WriteFile(filepath.Join(root, "unrelated.txt"), []byte("staged already\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCfg(t, root, "add", "unrelated.txt")
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("keep\n"), 0o644); err != nil {
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
	// The commit records keep.txt only; unrelated.txt is still a staged index entry, not committed.
	files := gitCfg(t, root, "show", "--name-only", "--pretty=format:", "HEAD")
	if strings.Contains(files, "unrelated.txt") {
		t.Fatalf("commit swept in the pre-staged unrelated.txt: files=%q", files)
	}
	if !strings.Contains(files, "keep.txt") {
		t.Fatalf("commit missing the requested keep.txt: files=%q", files)
	}
	if st := gitCfg(t, root, "status", "--porcelain", "unrelated.txt"); st != "A  unrelated.txt" {
		t.Fatalf("unrelated.txt should remain staged (uncommitted), status=%q", st)
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

// The author override is actually applied to the commit metadata, not silently dropped.
func TestGitCommit_authorApplied(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	// A committer identity is still needed even when the author is overridden.
	gitCfg(t, root, "config", "user.email", "committer@example.com")
	gitCfg(t, root, "config", "user.name", "Committer")
	gitCfg(t, root, "config", "commit.gpgsign", "false")
	t.Setenv(envWorkspaceRoot, root)

	if err := os.WriteFile(filepath.Join(root, "fix.txt"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewRegistry().Dispatch(context.Background(), "commit", map[string]any{
		"message": "authored fix",
		"author":  "Ada Lovelace <ada@example.com>",
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := gitCfg(t, root, "log", "-1", "--pretty=%an <%ae>"); got != "Ada Lovelace <ada@example.com>" {
		t.Fatalf("commit author = %q, want the override", got)
	}
	// The committer is still the ambient identity — only authorship was overridden.
	if got := gitCfg(t, root, "log", "-1", "--pretty=%cn"); got != "Committer" {
		t.Fatalf("committer = %q, want the ambient identity", got)
	}
}

// A control character in the author is rejected before it can slip a newline into commit metadata.
func TestGitCommit_rejectsControlCharAuthor(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	if err := os.WriteFile(filepath.Join(root, "x.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := NewRegistry().Dispatch(context.Background(), "commit", map[string]any{
		"message": "msg",
		"author":  "Evil\nUser <e@example.com>",
	})
	if err == nil {
		t.Fatal("an author with a control character must be rejected")
	}
	if !strings.Contains(err.Error(), "control character") {
		t.Fatalf("error should name the control character, got: %v", err)
	}
	// The bad input never produced a commit: HEAD is still the seed.
	if n := gitCfg(t, root, "rev-list", "--count", "HEAD"); n != "1" {
		t.Fatalf("a rejected author must not commit; commit count = %q", n)
	}
}

// paths accepts a single string as well as an array, staging just that pathspec.
func TestGitCommit_pathsAsString(t *testing.T) {
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
	if _, _, err := NewRegistry().Dispatch(context.Background(), "commit", map[string]any{
		"message": "only keep.txt",
		"paths":   "keep.txt",
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if st := gitCfg(t, root, "status", "--porcelain"); st != "?? later.txt" {
		t.Fatalf("status = %q, want only later.txt left untracked", st)
	}
}

// A malformed paths entry (empty or containing a control character) is rejected.
func TestGitCommit_rejectsBadPaths(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	if err := os.WriteFile(filepath.Join(root, "x.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := map[string]any{
		"empty entry":        []any{"  "},
		"control-char entry": []any{"a\tb.txt"},
		"non-string entry":   []any{42},
		"wrong type":         42,
	}
	for name, paths := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := NewRegistry().Dispatch(context.Background(), "commit", map[string]any{
				"message": "msg",
				"paths":   paths,
			}); err == nil {
				t.Fatalf("paths %v must be rejected", paths)
			}
		})
	}
	// None of the rejected inputs produced a commit.
	if n := gitCfg(t, root, "rev-list", "--count", "HEAD"); n != "1" {
		t.Fatalf("rejected paths must not commit; commit count = %q", n)
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

func TestGitStatus_listsDirtyPaths(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, err := NewRegistry().Dispatch(context.Background(), "status", nil)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	paths, _ := out["paths"].([]string)
	got := map[string]bool{}
	for _, p := range paths {
		got[p] = true
	}
	if !got["README.md"] || !got["new.txt"] {
		t.Fatalf("paths = %v, want README.md and new.txt", paths)
	}
	files, _ := out["files"].([]map[string]any)
	byPath := map[string]string{}
	for _, f := range files {
		p, _ := f["path"].(string)
		st, _ := f["status"].(string)
		byPath[p] = st
	}
	if byPath["README.md"] != "modified" {
		t.Fatalf("README.md status = %q, want modified", byPath["README.md"])
	}
	if byPath["new.txt"] != "untracked" {
		t.Fatalf("new.txt status = %q, want untracked", byPath["new.txt"])
	}
}

func TestGitStatus_cleanEmpty(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	out, _, err := NewRegistry().Dispatch(context.Background(), "status", nil)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	paths, _ := out["paths"].([]string)
	if len(paths) != 0 {
		t.Fatalf("clean tree paths = %v, want empty", paths)
	}
}

func TestGitDiff_workingTree(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, err := NewRegistry().Dispatch(context.Background(), "diff", nil)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	diff, _ := out["diff"].(string)
	if !strings.Contains(diff, "README.md") || !strings.Contains(diff, "+changed") {
		t.Fatalf("diff missing working-tree hunk:\n%s", diff)
	}
	if out["truncated"] != false {
		t.Fatalf("truncated = %v, want false", out["truncated"])
	}
}

func TestGitDiff_pathsScope(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("readme-changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "other.txt"), []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCfg(t, root, "add", "other.txt")
	gitCfg(t, root, "commit", "-m", "other")
	if err := os.WriteFile(filepath.Join(root, "other.txt"), []byte("other-changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, err := NewRegistry().Dispatch(context.Background(), "diff", map[string]any{"paths": []any{"README.md"}})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	diff, _ := out["diff"].(string)
	if !strings.Contains(diff, "README.md") {
		t.Fatalf("scoped diff missing README.md:\n%s", diff)
	}
	if strings.Contains(diff, "other.txt") {
		t.Fatalf("scoped diff leaked other.txt:\n%s", diff)
	}
}

func TestGitDiff_base(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	gitCfg(t, root, "switch", "-c", "feature")
	if err := os.WriteFile(filepath.Join(root, "feature.txt"), []byte("feat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCfg(t, root, "add", "feature.txt")
	gitCfg(t, root, "commit", "-m", "feature")
	out, _, err := NewRegistry().Dispatch(context.Background(), "diff", map[string]any{"base": "main"})
	if err != nil {
		t.Fatalf("diff base: %v", err)
	}
	diff, _ := out["diff"].(string)
	if !strings.Contains(diff, "feature.txt") || !strings.Contains(diff, "+feat") {
		t.Fatalf("base diff missing feature.txt:\n%s", diff)
	}
	if out["base"] != "main" {
		t.Fatalf("base = %v, want main", out["base"])
	}
}

func TestGitDiff_staged(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCfg(t, root, "add", "README.md")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("staged\nunstaged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, err := NewRegistry().Dispatch(context.Background(), "diff", map[string]any{"staged": true})
	if err != nil {
		t.Fatalf("diff staged: %v", err)
	}
	diff, _ := out["diff"].(string)
	if !strings.Contains(diff, "+staged") {
		t.Fatalf("staged diff missing staged hunk:\n%s", diff)
	}
	if strings.Contains(diff, "+unstaged") {
		t.Fatalf("staged diff leaked unstaged hunk:\n%s", diff)
	}
	if out["staged"] != true {
		t.Fatalf("staged = %v, want true", out["staged"])
	}
}

func TestGitDiff_invalidBase(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	if _, _, err := NewRegistry().Dispatch(context.Background(), "diff", map[string]any{"base": "--output=/tmp/x"}); err == nil {
		t.Fatal("expected invalid base to be rejected")
	}
}

func TestGitStatus_pathsScope(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "skip.txt"), []byte("nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, err := NewRegistry().Dispatch(context.Background(), "status", map[string]any{"paths": []any{"README.md"}})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	paths, _ := out["paths"].([]string)
	for _, p := range paths {
		if p == "skip.txt" {
			t.Fatalf("scoped status leaked skip.txt: %v", paths)
		}
	}
	found := false
	for _, p := range paths {
		if p == "README.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("scoped status missing README.md: %v", paths)
	}
}

func TestGitDiff_missingRoot(t *testing.T) {
	t.Setenv(envWorkspaceRoot, "")
	if _, _, err := NewRegistry().Dispatch(context.Background(), "diff", nil); err == nil {
		t.Fatal("expected an error when the workspace root is unset")
	}
	if _, _, err := NewRegistry().Dispatch(context.Background(), "status", nil); err == nil {
		t.Fatal("expected an error when the workspace root is unset")
	}
}

func TestCapBuffer_truncatesDuringWrite(t *testing.T) {
	c := capBuffer{max: 8}
	n, err := c.Write([]byte("abcdefghijklmnop"))
	if err != nil || n != 16 {
		t.Fatalf("Write n=%d err=%v, want n=16 (full input accepted so the pipe never blocks)", n, err)
	}
	if !c.truncated || string(c.buf) != "abcdefgh" {
		t.Fatalf("buf=%q truncated=%v, want first 8 bytes kept", c.buf, c.truncated)
	}
	n, err = c.Write([]byte("MORE"))
	if err != nil || n != 4 {
		t.Fatalf("second Write n=%d err=%v, want n=4 discarded", n, err)
	}
	if string(c.buf) != "abcdefgh" {
		t.Fatalf("buf grew after cap: %q", c.buf)
	}
}

func TestParseGitStatusPorcelainZ_unusualNames(t *testing.T) {
	// A modified file literally named "a -> b" must not be parsed as a rename from "a" to "b".
	out := " M a -> b\x00?? has space.txt\x00?? has\\slash.txt\x00?? café.txt\x00?? tab\tname.txt\x00?? line\nbreak.txt\x00"
	files := parseGitStatusPorcelainZ(out)
	byPath := map[string]map[string]any{}
	for _, f := range files {
		p, _ := f["path"].(string)
		byPath[p] = f
	}
	want := []string{"a -> b", "has space.txt", `has\slash.txt`, "café.txt", "tab\tname.txt", "line\nbreak.txt"}
	for _, p := range want {
		if byPath[p] == nil {
			t.Fatalf("missing path %q in %#v", p, files)
		}
	}
	arrow := byPath["a -> b"]
	if _, ok := arrow["from"]; ok {
		t.Fatalf("file named %q must not report a fake rename from=%v", "a -> b", arrow["from"])
	}
	if arrow["status"] != "modified" {
		t.Fatalf("a -> b status = %v, want modified", arrow["status"])
	}
}

func TestParseGitStatusPorcelainZ_renameTwoPathForm(t *testing.T) {
	// git status --porcelain=v1 -z emits PATH NUL ORIG_PATH (current path first).
	out := "R  a -> b\x00README.md\x00"
	files := parseGitStatusPorcelainZ(out)
	if len(files) != 1 {
		t.Fatalf("files = %#v, want one rename", files)
	}
	f := files[0]
	if f["path"] != "a -> b" || f["from"] != "README.md" || f["status"] != "renamed" {
		t.Fatalf("rename entry %#v, want path=a -> b from=README.md status=renamed", f)
	}
}

func TestGitDiff_capsBytesDuringIO(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	// Many short unique lines, not one giant line: some git builds report a 1 MiB
	// line as binary ("Binary files differ"), which is far under the cap and never
	// exercises the I/O truncate path. CombinedOutput would still allocate the
	// whole text diff. A working tree only ~1 MiB+4KiB produced a unified diff
	// just under the cap on Ubuntu CI (truncated=false). Write well past 1 MiB
	// with unique lines so the text diff cannot land under the bound.
	var b strings.Builder
	i := 0
	for b.Len() < maxGitDiffBytes*3 {
		b.WriteString(strings.Repeat("x", 48))
		b.WriteByte('-')
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('\n')
		i++
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, err := NewRegistry().Dispatch(context.Background(), "diff", nil)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if out["truncated"] != true {
		t.Fatalf("truncated = %v, want true (diff prefix %q)", out["truncated"], truncPrefix(out["diff"], 120))
	}
	diff, _ := out["diff"].(string)
	if len(diff) != maxGitDiffBytes {
		t.Fatalf("diff bytes = %d, want exactly the I/O cap %d", len(diff), maxGitDiffBytes)
	}
}

// TestGitDiff_keepsTailOfNormalSizeOutput is the regression for runGitCapped losing buffered tail
// data: calling cmd.Wait before the pipe readers finished closed the read ends, so a diff well
// under the cap (but larger than one pipe buffer, 64 KiB) could come back missing its final lines
// with truncated=false. The last line of the file must be present.
func TestGitDiff_keepsTailOfNormalSizeOutput(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	for _, size := range []int{70 << 10, 200 << 10, 600 << 10} {
		var b strings.Builder
		i := 0
		for b.Len() < size {
			b.WriteString(strings.Repeat("y", 48))
			b.WriteByte('-')
			b.WriteString(strconv.Itoa(i))
			b.WriteByte('\n')
			i++
		}
		b.WriteString("TAIL-SENTINEL-LINE\n")
		if err := os.WriteFile(filepath.Join(root, "README.md"), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		for range 20 {
			out, _, err := NewRegistry().Dispatch(context.Background(), "diff", nil)
			if err != nil {
				t.Fatalf("diff: %v", err)
			}
			if out["truncated"] != false {
				t.Fatalf("size %d: truncated = %v, want false (diff is far under the cap)", size, out["truncated"])
			}
			diff, _ := out["diff"].(string)
			if !strings.HasSuffix(diff, "+TAIL-SENTINEL-LINE\n") {
				t.Fatalf("size %d: diff lost its tail (len %d, last bytes %q)", size, len(diff), diff[max(0, len(diff)-60):])
			}
		}
	}
}

// writeMarkerScript writes an executable shell helper that appends a line to marker and then runs
// body, returning its path. Callers skip on Windows (no /bin/sh) before calling it.
func writeMarkerScript(t *testing.T, dir, name, marker, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	src := "#!/bin/sh\necho ran >> '" + marker + "'\n" + body + "\n"
	if err := os.WriteFile(path, []byte(src), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func markerRan(marker string) bool {
	_, err := os.Stat(marker)
	return err == nil
}

// plainGit runs git without the read-only hardening, as a positive control that a planted helper
// really is reachable on this git build (otherwise "not run" would prove nothing).
func plainGit(dir string, args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	_ = cmd.Run()
}

func TestGitDiff_doesNotRunTextconv(t *testing.T) {
	requireGit(t)
	if runtime.GOOS == "windows" {
		t.Skip("needs a /bin/sh textconv helper")
	}
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	helpers := t.TempDir() // outside the repo so the helper and marker are not part of the delta
	marker := filepath.Join(helpers, "textconv.ran")
	script := writeMarkerScript(t, helpers, "tc.sh", marker, `cat "$1"`)
	if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("*.md diff=planted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCfg(t, root, "add", ".gitattributes")
	gitCfg(t, root, "commit", "-m", "attrs")
	gitCfg(t, root, "config", "diff.planted.textconv", "'"+script+"'")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plainGit(root, "diff", "HEAD")
	if !markerRan(marker) {
		t.Skip("this git build did not run the planted textconv driver; cannot exercise the guard")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	for _, with := range []map[string]any{nil, {"staged": true}, {"base": "main"}} {
		out, _, err := NewRegistry().Dispatch(context.Background(), "diff", with)
		if err != nil {
			t.Fatalf("diff %v: %v", with, err)
		}
		if markerRan(marker) {
			t.Fatalf("git.diff %v executed the repository's textconv driver", with)
		}
		if with == nil {
			if diff, _ := out["diff"].(string); !strings.Contains(diff, "+changed") {
				t.Fatalf("diff missing raw hunk:\n%s", diff)
			}
		}
	}
}

func TestGitStatusAndDiff_doNotRunFsmonitor(t *testing.T) {
	requireGit(t)
	if runtime.GOOS == "windows" {
		t.Skip("needs a /bin/sh fsmonitor hook")
	}
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	helpers := t.TempDir()
	marker := filepath.Join(helpers, "fsmonitor.ran")
	// The v1/v2 hook protocols read a token argument and print a NUL-separated path list.
	script := writeMarkerScript(t, helpers, "fsm.sh", marker, `printf '/\0'`)
	gitCfg(t, root, "config", "core.fsmonitor", "'"+script+"'")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plainGit(root, "status", "--porcelain")
	if !markerRan(marker) {
		t.Skip("this git build did not run the planted fsmonitor hook; cannot exercise the guard")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	out, _, err := NewRegistry().Dispatch(context.Background(), "status", nil)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if markerRan(marker) {
		t.Fatal("git.status executed the repository's core.fsmonitor helper")
	}
	if paths, _ := out["paths"].([]string); len(paths) != 1 || paths[0] != "README.md" {
		t.Fatalf("status paths = %v, want [README.md]", paths)
	}
	if _, _, err := NewRegistry().Dispatch(context.Background(), "diff", nil); err != nil {
		t.Fatalf("diff: %v", err)
	}
	if markerRan(marker) {
		t.Fatal("git.diff executed the repository's core.fsmonitor helper")
	}
}

// TestGitStatusAndDiff_doNotRewriteIndex: plain `git status`/`git diff` opportunistically refresh
// stat data and rewrite .git/index; the workspace.read ops must leave it byte-for-byte alone.
func TestGitStatusAndDiff_doNotRewriteIndex(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	readme := filepath.Join(root, "README.md")
	// Same content, newer mtime: the index entry is stat-dirty, so a refresh would rewrite it.
	if err := os.Chtimes(readme, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(root, ".git", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"status", "diff"} {
		if _, _, err := NewRegistry().Dispatch(context.Background(), op, nil); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		after, err := os.ReadFile(indexPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Fatalf("git.%s rewrote .git/index", op)
		}
	}
}

func truncPrefix(v any, n int) string {
	s, _ := v.(string)
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func TestGitStatus_unusualFilenames(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	names := []string{
		"has space.txt",
		"a -> b",
		`has\slash.txt`,
		"café.txt",
		"tab\tname.txt",
		"line\nbreak.txt",
	}
	created := make([]string, 0, len(names))
	for _, name := range names {
		if !gitFilenameLegalOnOS(name) {
			continue
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte("x\n"), 0o644); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
		created = append(created, name)
	}
	if len(created) == 0 {
		t.Fatal("no unusual filenames were creatable on this OS")
	}
	names = created
	out, _, err := NewRegistry().Dispatch(context.Background(), "status", nil)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	got := map[string]map[string]any{}
	files, _ := out["files"].([]map[string]any)
	for _, f := range files {
		p, _ := f["path"].(string)
		got[p] = f
	}
	for _, name := range names {
		f := got[name]
		if f == nil {
			t.Fatalf("missing path %q in %#v", name, files)
		}
		if _, ok := f["from"]; ok {
			t.Fatalf("untracked %q reported a fake rename from=%v", name, f["from"])
		}
		if f["status"] != "untracked" {
			t.Fatalf("%q status = %v, want untracked", name, f["status"])
		}
	}
}

func TestGitStatus_realRenameKeepsBothPaths(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	dst := "a -> b"
	if !gitFilenameLegalOnOS(dst) {
		dst = "a to b"
	}
	gitCfg(t, root, "mv", "--", "README.md", dst)
	out, _, err := NewRegistry().Dispatch(context.Background(), "status", nil)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	files, _ := out["files"].([]map[string]any)
	var rename map[string]any
	for _, f := range files {
		if f["status"] == "renamed" {
			rename = f
			break
		}
	}
	if rename == nil {
		t.Fatalf("expected a renamed entry, got %#v", files)
	}
	if rename["from"] != "README.md" || rename["path"] != dst {
		t.Fatalf("rename %#v, want from=README.md path=%s", rename, dst)
	}
}

// TestGitStatusAndDiff_doNotRunPostIndexChangeHook: git diff (2.34) writes a refreshed index even
// with --no-optional-locks, and every index write runs post-index-change. The write lands in the
// throwaway index copy, but the hook — from .git/hooks or from core.hooksPath — must not run.
func TestGitStatusAndDiff_doNotRunPostIndexChangeHook(t *testing.T) {
	requireGit(t)
	if runtime.GOOS == "windows" {
		t.Skip("needs a /bin/sh hook")
	}
	for _, tc := range []struct {
		name     string
		hooksDir func(t *testing.T, root string) string
	}{
		{"dot-git-hooks", func(t *testing.T, root string) string {
			return filepath.Join(root, ".git", "hooks")
		}},
		{"core.hooksPath", func(t *testing.T, root string) string {
			dir := t.TempDir() // outside the repo, like a shared hooks directory
			gitCfg(t, root, "config", "core.hooksPath", dir)
			return dir
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initRepoWithCommit(t)
			t.Setenv(envWorkspaceRoot, root)
			hooks := tc.hooksDir(t, root)
			if err := os.MkdirAll(hooks, 0o755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "post-index-change.ran")
			writeMarkerScript(t, hooks, "post-index-change", marker, "true")
			readme := filepath.Join(root, "README.md")
			// Same content, newer mtime: the index entry is stat-dirty, so git refreshes and writes it.
			bump := func(d time.Duration) {
				t.Helper()
				ts := time.Now().Add(d)
				if err := os.Chtimes(readme, ts, ts); err != nil {
					t.Fatal(err)
				}
			}

			bump(time.Hour)
			plainGit(root, "diff", "HEAD")
			if !markerRan(marker) {
				t.Skip("this git build did not run the planted post-index-change hook; cannot exercise the guard")
			}
			if err := os.Remove(marker); err != nil {
				t.Fatal(err)
			}

			for i, op := range []string{"status", "diff"} {
				bump(time.Duration(i+2) * time.Hour) // the control refreshed the real index; dirty it again
				out, _, err := NewRegistry().Dispatch(context.Background(), op, nil)
				if err != nil {
					t.Fatalf("%s: %v", op, err)
				}
				if markerRan(marker) {
					t.Fatalf("git.%s executed the repository's post-index-change hook", op)
				}
				if op == "diff" {
					if diff, _ := out["diff"].(string); diff != "" {
						t.Fatalf("stat-only change should diff empty, got:\n%s", diff)
					}
				}
			}
		})
	}
}

// TestGitDiff_doesNotRunSubmoduleHelpers: diff.submodule=diff makes git diff spawn a nested diff
// inside each submodule that does not inherit --no-textconv/--no-ext-diff; --submodule=short must
// keep the submodule's textconv driver and diff.external from running.
func TestGitDiff_doesNotRunSubmoduleHelpers(t *testing.T) {
	requireGit(t)
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh helpers")
	}
	sub := initRepoWithCommit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	gitCfg(t, root, "-c", "protocol.file.allow=always", "submodule", "add", sub, "sub")
	gitCfg(t, root, "commit", "-m", "add submodule")
	helpers := t.TempDir()
	marker := filepath.Join(helpers, "submodule-helper.ran")
	tc := writeMarkerScript(t, helpers, "tc.sh", marker, `cat "$1"`)
	ext := writeMarkerScript(t, helpers, "ext.sh", marker, "true")
	subDir := filepath.Join(root, "sub")
	if err := os.WriteFile(filepath.Join(subDir, ".gitattributes"), []byte("* diff=planted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "README.md"), []byte("seed\nchanged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCfg(t, subDir, "config", "diff.planted.textconv", "'"+tc+"'")
	gitCfg(t, root, "config", "diff.submodule", "diff")

	for _, helper := range []string{"textconv", "diff.external"} {
		if helper == "diff.external" {
			gitCfg(t, subDir, "config", "--unset", "diff.planted.textconv")
			gitCfg(t, subDir, "config", "diff.external", "'"+ext+"'")
		}
		plainGit(root, "diff", "--no-textconv", "--no-ext-diff", "HEAD")
		if !markerRan(marker) {
			t.Skipf("this git build did not run the submodule %s via diff.submodule=diff; cannot exercise the guard", helper)
		}
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
		out, _, err := NewRegistry().Dispatch(context.Background(), "diff", nil)
		if err != nil {
			t.Fatalf("diff: %v", err)
		}
		if markerRan(marker) {
			t.Fatalf("git.diff executed the submodule's %s", helper)
		}
		if diff, _ := out["diff"].(string); !strings.Contains(diff, "Subproject commit") || !strings.Contains(diff, "-dirty") {
			t.Fatalf("diff should report the dirty submodule in short form:\n%s", diff)
		}
	}
}

// TestGitDiff_basePathIsNotPathspec: a base that names an existing directory or file (and not a
// ref) must be a clean error, not silently become `git diff <path>` (worktree vs index, scoped).
func TestGitDiff_basePathIsNotPathspec(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "a.md"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCfg(t, root, "add", "docs")
	gitCfg(t, root, "commit", "-m", "docs")
	if err := os.WriteFile(filepath.Join(root, "docs", "a.md"), []byte("a\nstaged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCfg(t, root, "add", "docs")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"docs", "README.md", "docs/a.md"} {
		for _, staged := range []bool{false, true} {
			out, _, err := NewRegistry().Dispatch(context.Background(), "diff", map[string]any{"base": base, "staged": staged})
			if err == nil {
				t.Fatalf("base %q staged=%v: want an error, got %#v", base, staged, out)
			}
			if !strings.Contains(err.Error(), "is not a commit") {
				t.Fatalf("base %q staged=%v: error %q should say base is not a commit", base, staged, err)
			}
		}
	}
}

// TestGitDiff_baseUsesMergeBase: base is PR-style — the diff is against the merge base, so a
// commit main gains after the branch forked must not show up (as a deletion) in "the change".
func TestGitDiff_baseUsesMergeBase(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	fork := gitCfg(t, root, "rev-parse", "HEAD")
	gitCfg(t, root, "switch", "-c", "feature")
	if err := os.WriteFile(filepath.Join(root, "feature.txt"), []byte("feat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCfg(t, root, "add", "feature.txt")
	gitCfg(t, root, "commit", "-m", "feature")
	// main moves on after the fork.
	gitCfg(t, root, "switch", "main")
	if err := os.WriteFile(filepath.Join(root, "main-only.txt"), []byte("main work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCfg(t, root, "add", "main-only.txt")
	gitCfg(t, root, "commit", "-m", "main work")
	gitCfg(t, root, "switch", "feature")
	// An uncommitted edit on the branch is part of the change too.
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("seed\nwip\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _, err := NewRegistry().Dispatch(context.Background(), "diff", map[string]any{"base": "main"})
	if err != nil {
		t.Fatalf("diff base: %v", err)
	}
	diff, _ := out["diff"].(string)
	if strings.Contains(diff, "main-only.txt") || strings.Contains(diff, "main work") {
		t.Fatalf("merge-base diff leaked main's post-fork commit:\n%s", diff)
	}
	if !strings.Contains(diff, "+feat") || !strings.Contains(diff, "+wip") {
		t.Fatalf("merge-base diff missing the branch's committed and uncommitted changes:\n%s", diff)
	}
	if out["merge_base"] != fork {
		t.Fatalf("merge_base = %v, want fork point %s", out["merge_base"], fork)
	}
	if out["base"] != "main" {
		t.Fatalf("base = %v, want main", out["base"])
	}

	// staged + base: the index vs the same merge base (the wip edit is unstaged, so absent).
	out, _, err = NewRegistry().Dispatch(context.Background(), "diff", map[string]any{"base": "main", "staged": true})
	if err != nil {
		t.Fatalf("diff base staged: %v", err)
	}
	diff, _ = out["diff"].(string)
	if strings.Contains(diff, "main-only.txt") || strings.Contains(diff, "+wip") || !strings.Contains(diff, "+feat") {
		t.Fatalf("staged merge-base diff wrong:\n%s", diff)
	}
}

func TestGitDiff_baseWithoutCommonAncestor(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	gitCfg(t, root, "switch", "--orphan", "island")
	if err := os.WriteFile(filepath.Join(root, "island.txt"), []byte("i\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCfg(t, root, "add", "island.txt")
	gitCfg(t, root, "commit", "-m", "island")
	_, _, err := NewRegistry().Dispatch(context.Background(), "diff", map[string]any{"base": "main"})
	if err == nil || !strings.Contains(err.Error(), "no merge base") {
		t.Fatalf("err = %v, want a no-merge-base error", err)
	}
}

// TestGitDiff_unbornBranch: in a repository with no commits the default diff is against the
// empty tree (computed in the repository's own hash, so SHA-256 repositories work too).
func TestGitDiff_unbornBranch(t *testing.T) {
	requireGit(t)
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			root := t.TempDir()
			cmd := exec.Command("git", "init", "--object-format="+format, root)
			if out, err := cmd.CombinedOutput(); err != nil {
				if format == "sha256" {
					t.Skipf("git cannot create a sha256 repository: %v\n%s", err, out)
				}
				t.Fatalf("git init: %v\n%s", err, out)
			}
			gitCfg(t, root, "config", "core.autocrlf", "false")
			t.Setenv(envWorkspaceRoot, root)
			if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("staged\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCfg(t, root, "add", "README.md")
			if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("staged\nunstaged\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			out, _, err := NewRegistry().Dispatch(context.Background(), "diff", nil)
			if err != nil {
				t.Fatalf("diff: %v", err)
			}
			diff, _ := out["diff"].(string)
			if !strings.Contains(diff, "new file mode") || !strings.Contains(diff, "+staged") || !strings.Contains(diff, "+unstaged") {
				t.Fatalf("unborn diff should add the whole working-tree file:\n%s", diff)
			}
			if out["unborn"] != true {
				t.Fatalf("unborn = %v, want true", out["unborn"])
			}

			out, _, err = NewRegistry().Dispatch(context.Background(), "diff", map[string]any{"staged": true})
			if err != nil {
				t.Fatalf("diff staged: %v", err)
			}
			diff, _ = out["diff"].(string)
			if !strings.Contains(diff, "+staged") || strings.Contains(diff, "+unstaged") {
				t.Fatalf("unborn staged diff wrong:\n%s", diff)
			}

			_, _, err = NewRegistry().Dispatch(context.Background(), "diff", map[string]any{"base": "main"})
			if err == nil || !strings.Contains(err.Error(), "no commits yet") {
				t.Fatalf("unborn base err = %v, want a clear no-commits error", err)
			}
		})
	}
}

func TestParseGitStatusPorcelainZ_unmerged(t *testing.T) {
	var out strings.Builder
	pairs := []string{"DD", "AU", "UD", "UA", "DU", "AA", "UU"}
	for _, xy := range pairs {
		out.WriteString(xy + " f-" + xy + "\x00")
	}
	// Non-conflict neighbours keep their ordinary labels.
	out.WriteString("A  added.txt\x00 D gone.txt\x00MM both.txt\x00")
	files := parseGitStatusPorcelainZ(out.String())
	byPath := map[string]any{}
	for _, f := range files {
		byPath[f["path"].(string)] = f["status"]
	}
	for _, xy := range pairs {
		if got := byPath["f-"+xy]; got != "unmerged" {
			t.Errorf("%s status = %v, want unmerged", xy, got)
		}
	}
	for p, want := range map[string]string{"added.txt": "added", "gone.txt": "deleted", "both.txt": "modified"} {
		if byPath[p] != want {
			t.Errorf("%s status = %v, want %s", p, byPath[p], want)
		}
	}
}

func TestGitStatus_realMergeConflict(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	gitCfg(t, root, "switch", "-c", "other")
	for name, body := range map[string]string{"README.md": "other\n", "new.txt": "other new\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCfg(t, root, "add", "-A")
	gitCfg(t, root, "commit", "-m", "other")
	gitCfg(t, root, "switch", "main")
	for name, body := range map[string]string{"README.md": "mine\n", "new.txt": "my new\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCfg(t, root, "add", "-A")
	gitCfg(t, root, "commit", "-m", "mine")
	merge := exec.Command("git", "-c", "user.email=t@example.com", "-c", "user.name=Test", "merge", "other")
	merge.Dir = root
	if out, err := merge.CombinedOutput(); err == nil {
		t.Fatalf("expected a merge conflict, merge succeeded:\n%s", out)
	}

	out, _, err := NewRegistry().Dispatch(context.Background(), "status", nil)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	files, _ := out["files"].([]map[string]any)
	byPath := map[string]map[string]any{}
	for _, f := range files {
		byPath[f["path"].(string)] = f
	}
	want := map[string]string{"README.md": "UU", "new.txt": "AA"}
	for p, xy := range want {
		f := byPath[p]
		if f == nil {
			t.Fatalf("missing %s in %#v", p, files)
		}
		if f["status"] != "unmerged" || f["index"].(string)+f["worktree"].(string) != xy {
			t.Fatalf("%s = %#v, want status unmerged with XY %s", p, f, xy)
		}
	}
	// diff still works mid-merge and shows the conflict markers against HEAD.
	dout, _, err := NewRegistry().Dispatch(context.Background(), "diff", nil)
	if err != nil {
		t.Fatalf("diff during conflict: %v", err)
	}
	if diff, _ := dout["diff"].(string); !strings.Contains(diff, "+<<<<<<<") {
		t.Fatalf("diff during conflict should show markers:\n%s", diff)
	}
}

func TestParseHookConfigListing(t *testing.T) {
	out := "hook.audit.event\npost-index-change\x00" +
		"hook.audit.command\necho ran\x00" +
		"hook.a.b c.event\npost-index-change\x00" + // subsection with a dot and a space
		"hook.a.b c.command\ntrue\x00" +
		"hook.Solo.event\x00" + // value-less entry
		"hook.x.event\npre-commit\x00" +
		"hook.x.event\n\x00" + // empty value resets the list
		"hook.x.event\nweird=event\x00" +
		"hook.x.event\npost-index-change\x00"
	names, events, err := parseHookConfigListing(out)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(names, "|"), "Solo|a.b c|audit|x"; got != want {
		t.Fatalf("names = %q, want %q", got, want)
	}
	if got, want := strings.Join(events, "|"), "post-index-change|pre-commit"; got != want {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if _, _, err := parseHookConfigListing("hook.a=b.command\ntrue\x00"); err == nil || !strings.Contains(err.Error(), "'='") {
		t.Fatalf("name with '=': err = %v, want a fail-closed error", err)
	}
	if names, events, err := parseHookConfigListing(""); err != nil || len(names) != 0 || len(events) != 0 {
		t.Fatalf("empty listing: %v %v %v", names, events, err)
	}
}

// TestReadOnlyGit_listsConfiguredHooksFromEveryScope: the session disables every config-defined
// hook by name, from repository and global config alike, and git reads each -c key back under the
// same (dotted, spaced) name. This runs on any git: the listing and the -c round trip are plain
// config, even where hook.* has no meaning yet (git < 2.54).
func TestReadOnlyGit_listsConfiguredHooksFromEveryScope(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte("[hook \"from-global\"]\n\tevent = post-index-change\n\tcommand = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global) // git >= 2.32
	gitCfg(t, root, "config", "hook.audit.event", "post-index-change")
	gitCfg(t, root, "config", "hook.audit.command", "true")
	gitCfg(t, root, "config", "hook.a.b c.event", "pre-commit")
	gitCfg(t, root, "config", "hook.a.b c.command", "true")

	g, err := newReadOnlyGit(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer g.close()
	joined := strings.Join(g.hookOff, " ")
	for _, want := range []string{
		"-c hook.audit.enabled=false",
		"-c hook.a.b c.enabled=false",
		"-c hook.from-global.enabled=false",
		"-c hook.post-index-change.enabled=false",
		"-c hook.pre-commit.enabled=false",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("hookOff = %q, missing %q", joined, want)
		}
	}
	for _, name := range []string{"audit", "a.b c", "from-global", "post-index-change"} {
		out, _, err := g.run(context.Background(), maxGitOIDOutput, "config", "--get", "hook."+name+".enabled")
		if err != nil || strings.TrimSpace(out) != "false" {
			t.Fatalf("hook.%s.enabled in the session = %q, %v; want false", name, out, err)
		}
	}

	// A name -c cannot express fails the op closed.
	gitCfg(t, root, "config", "hook.a=b.event", "post-index-change")
	t.Setenv(envWorkspaceRoot, root)
	if _, _, err := NewRegistry().Dispatch(context.Background(), "diff", nil); err == nil || !strings.Contains(err.Error(), "'='") {
		t.Fatalf("diff with an inexpressible hook name: err = %v, want fail closed", err)
	}
}

// TestGitStatusAndDiff_doNotRunConfigDefinedHooks: since git 2.54 hooks can be defined in config
// (hook.<name>.event / hook.<name>.command, from any scope), and core.hooksPath does not affect
// them. git diff's index write runs a configured post-index-change hook, so it must be disabled by
// name (git 2.54) and by event (git >= 2.55). Skips where plain git does not run the planted hooks
// (git < 2.54, e.g. the 2.34 of Ubuntu 22.04).
func TestGitStatusAndDiff_doNotRunConfigDefinedHooks(t *testing.T) {
	requireGit(t)
	if runtime.GOOS == "windows" {
		t.Skip("hook commands are shell one-liners")
	}
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	markers := t.TempDir()
	marker := func(name string) string { return filepath.Join(markers, name+".ran") }
	global := filepath.Join(t.TempDir(), "gitconfig")
	globalCfg := "[hook \"from-global\"]\n\tevent = post-index-change\n\tcommand = echo ran >> '" + marker("from-global") + "'\n"
	if err := os.WriteFile(global, []byte(globalCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	for _, name := range []string{"x", "a.b"} { // "a.b": a friendly name containing a dot
		gitCfg(t, root, "config", "hook."+name+".event", "post-index-change")
		gitCfg(t, root, "config", "hook."+name+".command", "echo ran >> '"+marker(name)+"'")
	}
	all := []string{"x", "a.b", "from-global"}
	readme := filepath.Join(root, "README.md")
	bump := func(d time.Duration) {
		t.Helper()
		ts := time.Now().Add(d)
		if err := os.Chtimes(readme, ts, ts); err != nil {
			t.Fatal(err)
		}
	}

	bump(time.Hour)
	plainGit(root, "diff", "HEAD")
	fired := 0
	for _, n := range all {
		if markerRan(marker(n)) {
			fired++
		}
	}
	if fired == 0 {
		t.Skip("this git build does not run config-defined hooks (git < 2.54); cannot exercise the guard")
	}
	for _, n := range all {
		if !markerRan(marker(n)) {
			t.Fatalf("control: plain git ran some configured hooks but not %q", n)
		}
		if err := os.Remove(marker(n)); err != nil {
			t.Fatal(err)
		}
	}

	for i, op := range []string{"status", "diff"} {
		bump(time.Duration(i+2) * time.Hour)
		out, _, err := NewRegistry().Dispatch(context.Background(), op, nil)
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		for _, n := range all {
			if markerRan(marker(n)) {
				t.Fatalf("git.%s executed the config-defined post-index-change hook %q", op, n)
			}
		}
		if op == "diff" {
			if diff, _ := out["diff"].(string); diff != "" {
				t.Fatalf("stat-only change should diff empty, got:\n%s", diff)
			}
		}
	}
}

// fileURL is a file:// URL for a local path, so git uses its transport (a plain local path makes
// clone ignore --depth).
func fileURL(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows: C:/x -> /C:/x
	}
	return "file://" + p
}

// TestGitDiff_baseInShallowClone: in a shallow clone whose fetched history does not reach the fork
// point, merge-base finds nothing although the histories are related. The op still fails closed but
// must say that the clone is shallow, not that the branch is unrelated to base.
func TestGitDiff_baseInShallowClone(t *testing.T) {
	requireGit(t)
	upstream := initRepoWithCommit(t)
	commitFile := func(dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCfg(t, dir, "add", name)
		gitCfg(t, dir, "commit", "-m", name)
	}
	commitFile(upstream, "b.txt", "b\n")
	parent := t.TempDir()
	gitCfg(t, parent, "clone", "--depth=1", fileURL(upstream), "clone")
	clone := filepath.Join(parent, "clone")
	if got := gitCfg(t, clone, "rev-parse", "--is-shallow-repository"); got != "true" {
		t.Skipf("clone --depth=1 did not produce a shallow repository (%q)", got)
	}
	// main moves on upstream; a depth-1 fetch brings its tip without the history back to the fork.
	commitFile(upstream, "c.txt", "c\n")
	gitCfg(t, clone, "fetch", "--depth=1", "origin", "main")
	t.Setenv(envWorkspaceRoot, clone)

	_, _, err := NewRegistry().Dispatch(context.Background(), "diff", map[string]any{"base": "origin/main"})
	if err == nil {
		t.Fatal("diff base in a shallow clone without the fork point: want an error")
	}
	if !strings.Contains(err.Error(), "shallow clone") || !strings.Contains(err.Error(), "--unshallow") || strings.Contains(err.Error(), "unrelated") {
		t.Fatalf("err = %v, want a shallow-clone diagnosis", err)
	}
}

// TestGitDiff_mergeBaseFailureIsNotNoMergeBase: only merge-base's exit status 1 means "no common
// ancestor". Any other failure — here a missing commit object on the walk — reports git's error.
func TestGitDiff_mergeBaseFailureIsNotNoMergeBase(t *testing.T) {
	requireGit(t)
	root := initRepoWithCommit(t)
	t.Setenv(envWorkspaceRoot, root)
	fork := gitCfg(t, root, "rev-parse", "HEAD")
	for _, br := range []string{"main", "feature"} {
		if br == "feature" {
			gitCfg(t, root, "switch", "-c", "feature", fork)
		}
		if err := os.WriteFile(filepath.Join(root, br+".txt"), []byte(br+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCfg(t, root, "add", br+".txt")
		gitCfg(t, root, "commit", "-m", br)
	}
	// Remove the fork commit's loose object: both tips still resolve, but the merge-base walk
	// reaches the missing parent and git fails with something other than exit status 1.
	obj := filepath.Join(root, ".git", "objects", fork[:2], fork[2:])
	if err := os.Chmod(obj, 0o644); err != nil {
		t.Skipf("fork commit is not a loose object: %v", err)
	}
	if err := os.Remove(obj); err != nil {
		t.Fatal(err)
	}
	_, _, err := NewRegistry().Dispatch(context.Background(), "diff", map[string]any{"base": "main"})
	if err == nil {
		t.Fatal("diff base with a corrupt history: want an error")
	}
	if strings.Contains(err.Error(), "no merge base") || !strings.Contains(err.Error(), "merge base of base") {
		t.Fatalf("err = %v, want git's merge-base failure, not a no-merge-base diagnosis", err)
	}
}
