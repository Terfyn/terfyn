package native

import (
	"context"
	"errors"
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

// gitCommit stages working-tree changes and commits them (issue #528). It is the missing verb
// between the agents' edits (which only touch the working tree) and push_branch (which pushes a
// ref): without a commit the branch still sits at its base and pull_request.create fails 422
// "No commits". Like create_branch it is a local repository.write — it runs unattended; the gated
// publication ops (push_branch, pull_request.create) are unchanged.
//
// message is required. paths (optional) restricts staging and the commit to those pathspecs;
// absent, it stages ALL working-tree changes (git add -A) — including any non-ignored incidental
// dirt in the root, not just the agent's edits, so an unattended caller that cannot assume a clean
// tree should pass explicit paths. author (optional, "Name <email>") overrides the committer's
// authorship, else the ambient git config identity is used. "Nothing to commit" is a graceful,
// non-fatal result ({committed:false, reason:"nothing to commit"}) so a no-op change — the agent
// decided the deliverable already existed — does not crash the run; the workflow can branch on it
// before pushing.
func gitCommit(ctx context.Context, with map[string]any) (map[string]any, error) {
	root, err := workspaceRoot(ctx)
	if err != nil {
		return nil, err
	}
	message, err := stringFromWith(with, "message")
	if err != nil {
		return nil, fmt.Errorf("native: commit %w", err)
	}
	paths, err := pathsFromWith(with, "paths")
	if err != nil {
		return nil, fmt.Errorf("native: commit: %w", err)
	}
	rawAuthor, _, err := optionalStringFromWith(with, "author")
	if err != nil {
		return nil, fmt.Errorf("native: commit: %w", err)
	}
	author, err := validateAuthor(rawAuthor)
	if err != nil {
		return nil, fmt.Errorf("native: commit: %w", err)
	}

	// Stage: an explicit pathspec set restricts staging (git add -- <paths>), else stage everything
	// including untracked (git add -A). Pathspecs go after "--" so none can be read as an option; note
	// "--" ends option parsing, not git pathspec magic (a leading ":" like :(exclude) is still honored),
	// so these are pathspecs, not guaranteed-literal paths — not an escalation for in-sandbox git add.
	if len(paths) > 0 {
		addArgs := append([]string{"add", "--"}, paths...)
		if _, err := runGit(ctx, root, addArgs...); err != nil {
			return nil, err
		}
	} else if _, err := runGit(ctx, root, "add", "-A"); err != nil {
		return nil, err
	}

	// Nothing to commit is a graceful outcome, not a failure: report it so the workflow can skip the
	// push instead of a 422 later. Scoped to the same pathspecs as the commit (against the index), so
	// unrelated staged or unstaged changes never count as — nor get swept into — "something to commit".
	staged, err := hasStagedChanges(ctx, root, paths)
	if err != nil {
		return nil, fmt.Errorf("native: commit: %w", err)
	}
	if !staged {
		return map[string]any{"committed": false, "reason": "nothing to commit"}, nil
	}

	args := []string{"commit", "-m", message}
	if author != "" {
		args = append(args, "--author="+author)
	}
	// Scope the commit to the requested pathspecs (after "--") so it records exactly what was staged
	// here and never sweeps in an unrelated pre-staged index entry; empty paths commits the whole index.
	if len(paths) > 0 {
		args = append(args, "--")
		args = append(args, paths...)
	}
	if _, err := runGit(ctx, root, args...); err != nil {
		return nil, err
	}
	sha := strings.TrimSpace(headSHA(ctx, root))
	out := map[string]any{"committed": true, "message": message}
	if sha != "" {
		out["sha"] = sha
	}
	return out, nil
}

// hasStagedChanges reports whether the index differs from HEAD (something is staged to commit),
// restricted to paths when non-empty so the check matches the scope of the commit that follows.
// `git diff --cached --quiet` exits 0 when the (scoped) index is clean and 1 when it has staged
// changes; any other exit is a real error. Distinguishing exit 1 from failure is why this does not
// go through runGit (which treats every non-zero exit as an error).
func hasStagedChanges(ctx context.Context, root string, paths []string) (bool, error) {
	args := []string{"diff", "--cached", "--quiet"}
	if len(paths) > 0 {
		args = append(args, "--")
		args = append(args, paths...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	err := cmd.Run()
	if err == nil {
		return false, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return true, nil
	}
	return false, fmt.Errorf("git diff --cached: %w", err)
}

// headSHA returns the current HEAD sha, or "" if it cannot be resolved. The commit already
// succeeded, so a failure here only omits the sha from the result — it is not a run-ending error.
func headSHA(ctx context.Context, root string) string {
	out, err := runGit(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return out
}

// pathsFromWith reads an optional pathspec list. It accepts a JSON array (arriving as []any) or a
// single string for convenience; absent/nil yields nil (stage everything). Each entry must be a
// non-empty string with no control characters — pathspecs are passed after "--", so this is
// hygiene, not injection defense.
func pathsFromWith(with map[string]any, key string) ([]string, error) {
	v, ok := with[key]
	if !ok || v == nil {
		return nil, nil
	}
	var raw []any
	switch t := v.(type) {
	case []any:
		raw = t
	case string:
		raw = []any{t}
	default:
		return nil, fmt.Errorf("field %q must be a string or an array of strings, got %T", key, v)
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("field %q entries must be strings, got %T", key, e)
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, fmt.Errorf("field %q entries must be non-empty", key)
		}
		for _, r := range s {
			if r < 0x20 || r == 0x7f {
				return nil, fmt.Errorf("field %q entry %q contains a control character", key, s)
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// validateAuthor accepts an empty override (use the ambient git identity) or a "Name <email>"
// string with no control characters. The value is passed as a single --author= argument, so this
// guards only against a newline or control char slipping into the commit metadata.
func validateAuthor(author string) (string, error) {
	author = strings.TrimSpace(author)
	if author == "" {
		return "", nil
	}
	for _, r := range author {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("author %q contains a control character", author)
		}
	}
	return author, nil
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

func dispatchGitCommit(ctx context.Context, with map[string]any, start time.Time) (map[string]any, ExecMeta, error) {
	out, err := gitCommit(ctx, with)
	meta := ExecMeta{DurationMs: time.Since(start).Milliseconds()}
	if err != nil {
		return nil, meta, err
	}
	return out, meta, nil
}
