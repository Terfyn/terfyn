package native

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Native git adapter (issue #331): exactly two operations — create a local branch and push a
// branch to a remote — so "propose a fix on a branch, human approves the push" is expressible with
// no custom tool. Deliberately narrow: no push to the default branch, no --force, no branch delete,
// no arbitrary git. push_branch is meant to sit in approvals.requiredFor, like
// pull_request.post_comment, so the run suspends for approval before anything leaves the machine.
//
// Both ops run in the workspace sandbox (TERFYN_WORKSPACE_ROOT, the same root the workspace adapter
// uses); push uses the ambient git credentials / GITHUB_TOKEN, like the github adapter's live path.
// The remote is TERFYN_GIT_REMOTE (default origin).
const envGitRemote = "TERFYN_GIT_REMOTE"

func gitRemote() string {
	r := strings.TrimSpace(os.Getenv(envGitRemote))
	if r == "" {
		return "origin"
	}
	return r
}

// validateBranchName rejects names that are unsafe as a git argument or refspec: a leading '-'
// (would be read as a flag), a ':' (a push refspec that could delete a ref), and characters git
// forbids in a ref. It is intentionally strict — a branch a fixer proposes is a simple name.
func validateBranchName(field, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("field %q is required", field)
	}
	if strings.HasPrefix(name, "-") {
		return "", fmt.Errorf("branch name %q may not start with '-'", name)
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".lock") {
		return "", fmt.Errorf("invalid branch name %q", name)
	}
	if strings.Contains(name, "..") || strings.Contains(name, "@{") {
		return "", fmt.Errorf("invalid branch name %q", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("branch name %q contains a control character", name)
		}
		switch r {
		case ' ', '\t', ':', '~', '^', '?', '*', '[', '\\':
			return "", fmt.Errorf("branch name %q contains an invalid character %q", name, r)
		}
	}
	return name, nil
}

// runGit runs git in the workspace root and returns combined output; a non-zero exit is an error
// carrying a truncated tail of the output. git args are passed as a slice (no shell), so a validated
// branch name cannot inject a flag or a second command.
func runGit(ctx context.Context, root string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	b, err := cmd.CombinedOutput()
	out := string(b)
	if err != nil {
		return out, fmt.Errorf("native: git %s: %w: %s", strings.Join(args, " "), err, truncateRunes(strings.TrimSpace(out), 512))
	}
	return out, nil
}

func gitCreateBranch(ctx context.Context, with map[string]any) (map[string]any, error) {
	root, err := workspaceRoot(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := stringFromWith(with, "name", "branch")
	if err != nil {
		return nil, fmt.Errorf("native: create_branch %w", err)
	}
	name, err := validateBranchName("name", raw)
	if err != nil {
		return nil, fmt.Errorf("native: create_branch: %w", err)
	}
	// reset (opt-in, destructive) force-recreates the branch at the start point, discarding any commits
	// on a prior attempt — usually what a re-run of a fix workflow wants (issue #517).
	reset, _, err := optionalBoolFromWith(with, "reset")
	if err != nil {
		return nil, fmt.Errorf("native: create_branch: %w", err)
	}
	// base is the optional start point (a ref/branch/sha) the branch is created or reset from; it is a
	// ref, so it accepts the same shape as a branch name (e.g. "main", "origin/main").
	rawBase, _, err := optionalStringFromWith(with, "base")
	if err != nil {
		return nil, fmt.Errorf("native: create_branch: %w", err)
	}
	var base string
	if rawBase != "" {
		if base, err = validateBranchName("base", rawBase); err != nil {
			return nil, fmt.Errorf("native: create_branch: %w", err)
		}
	}

	existed := branchExists(ctx, root, name)
	switch {
	case reset:
		// A reset must name an explicit start point. `switch -C <name>` with no start point resets to
		// the CURRENT HEAD — which, in the re-run topology this op exists for (the workspace is still on
		// the fix branch from a prior attempt), is a no-op that leaves the prior commits in place. So
		// require `base` and fail closed rather than silently claiming a reset that did nothing (#517).
		if base == "" {
			return nil, fmt.Errorf("native: create_branch: reset requires base (the ref to reset the branch to, e.g. base \"main\"); resetting to the current HEAD is a no-op when already on the branch")
		}
		// `switch -C <name> <base>` creates the branch or resets an existing one to the start point.
		if _, err := runGit(ctx, root, "switch", "-C", name, base); err != nil {
			return nil, err
		}
		return map[string]any{"branch": name, "created": !existed, "reset": true}, nil
	case existed:
		// Idempotent default: the branch already exists, so just switch to it (base is a create-time
		// start point and does not apply here). "already exists" is success, not a run-ending error.
		if _, err := runGit(ctx, root, "switch", name); err != nil {
			return nil, err
		}
		return map[string]any{"branch": name, "created": false}, nil
	default:
		args := []string{"switch", "-c", name}
		if base != "" {
			args = append(args, base)
		}
		if _, err := runGit(ctx, root, args...); err != nil {
			return nil, err
		}
		return map[string]any{"branch": name, "created": true}, nil
	}
}

// branchExists reports whether a local branch named `name` exists in the workspace.
func branchExists(ctx context.Context, root, name string) bool {
	_, err := runGit(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+name)
	return err == nil
}

func gitPushBranch(ctx context.Context, with map[string]any) (map[string]any, error) {
	root, err := workspaceRoot(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := stringFromWith(with, "branch", "name")
	if err != nil {
		return nil, fmt.Errorf("native: push_branch %w", err)
	}
	branch, err := validateBranchName("branch", raw)
	if err != nil {
		return nil, fmt.Errorf("native: push_branch: %w", err)
	}
	remote := gitRemote()
	// Enforce "no push to the default branch": a fix must land on a review branch, never directly
	// on the remote's default. Resolve the remote's default and refuse a push that targets it. If the
	// remote can't be queried, fall back to a conservative main/master denylist.
	if def := remoteDefaultBranch(ctx, root, remote); def != "" {
		if branch == def {
			return nil, fmt.Errorf("native: push_branch: refusing to push the default branch %q; push a fix branch", branch)
		}
	} else if branch == "main" || branch == "master" {
		return nil, fmt.Errorf("native: push_branch: refusing to push a likely default branch %q; push a fix branch", branch)
	}
	// Explicit src:dst refspec (both the validated branch) so the push always creates/updates
	// refs/heads/<branch> and can never be read as a delete (`:branch`) or a flag.
	refspec := "refs/heads/" + branch + ":refs/heads/" + branch
	if _, err := runGit(ctx, root, "push", remote, refspec); err != nil {
		return nil, err
	}
	return map[string]any{"branch": branch, "remote": remote, "pushed": true}, nil
}

// remoteDefaultBranch resolves the remote's default branch (the branch its HEAD points at) via
// ls-remote --symref, or "" when it cannot be determined. It reads the remote — the same round trip
// push makes — and never fails the push: an unresolved default falls back to the main/master denylist.
func remoteDefaultBranch(ctx context.Context, root, remote string) string {
	out, err := runGit(ctx, root, "ls-remote", "--symref", remote, "HEAD")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ref:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "ref:"))
		if len(fields) == 0 {
			continue
		}
		if b := strings.TrimPrefix(fields[0], "refs/heads/"); b != fields[0] {
			return b
		}
	}
	return ""
}

func dispatchGitCreateBranch(ctx context.Context, with map[string]any, start time.Time) (map[string]any, ExecMeta, error) {
	out, err := gitCreateBranch(ctx, with)
	meta := ExecMeta{DurationMs: time.Since(start).Milliseconds()}
	if err != nil {
		return nil, meta, err
	}
	return out, meta, nil
}

func dispatchGitPushBranch(ctx context.Context, with map[string]any, start time.Time) (map[string]any, ExecMeta, error) {
	out, err := gitPushBranch(ctx, with)
	meta := ExecMeta{DurationMs: time.Since(start).Milliseconds()}
	if err != nil {
		return nil, meta, err
	}
	return out, meta, nil
}
