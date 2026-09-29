package native

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Native git adapter (issue #331): a narrow set of git operations so "propose a fix on a branch,
// human approves the push" is expressible with no custom tool — plus read-only inspection so a
// Reviewer can grade the actual working-tree delta (issue #534), not the Implementer's summary.
// Deliberately narrow: no push to the default branch, no --force, no branch delete, no arbitrary
// git. push_branch is meant to sit in approvals.requiredFor, like pull_request.post_comment, so the
// run suspends for approval before anything leaves the machine.
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

// maxGitStderrBytes caps git diagnostic text on a failed capped run so stderr cannot grow
// unbounded either. The bound applies during I/O, matching stdout.
const maxGitStderrBytes = 512

// capBuffer keeps at most max bytes and discards the rest, recording truncated. Write always
// reports the full input length so the producer is never blocked by a full pipe after the cap.
type capBuffer struct {
	buf       []byte
	max       int
	truncated bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if c.max < 0 {
		c.max = 0
	}
	if !c.truncated {
		room := c.max - len(c.buf)
		if room > 0 {
			if len(p) > room {
				c.buf = append(c.buf, p[:room]...)
				c.truncated = true
			} else {
				c.buf = append(c.buf, p...)
			}
		} else if len(p) > 0 {
			c.truncated = true
		}
	}
	return len(p), nil
}

// readOnlyGitArgs prefixes args with the flags that keep an inspection command from executing
// repository-configured helpers or writing to .git. It is applied to every command a readOnlyGit
// session runs (git.diff and git.status, and the rev-parse / merge-base / hash-object lookups
// they make), not only to the subcommands' own flags. -c settings are passed to any child git
// process (e.g. a submodule's status) through GIT_CONFIG_PARAMETERS, so they cover submodules too:
//   - --no-pager: never spawn core.pager / pager.<cmd>.
//   - -c core.fsmonitor=false, -c core.useBuiltinFSMonitor=false: status/diff consult the
//     fsmonitor hook (an arbitrary command from .git/config) to decide which files to stat.
//   - hooks. git diff (2.34+) refreshes and writes the index even with --no-optional-locks, and
//     every index write runs the post-index-change hook. The write lands in the throwaway index
//     copy, but the hook would still run. git has two hook sources, and both are disabled:
//     (1) the hook directory (.git/hooks or core.hooksPath): -c core.hooksPath=<empty dir we
//     own>, so no hook file exists there (a directory rather than os.DevNull so the lookup
//     behaves the same on Windows); (2) since git 2.54, hooks defined in config
//     (hook.<name>.event + hook.<name>.command, from any scope: system, global, repository,
//     worktree, includes, or the environment), which core.hooksPath does not affect:
//     -c hook.post-index-change.enabled=false switches the whole event off on git >= 2.55 (older
//     git reads it as an unknown hook name and ignores it); -c hook..enabled=false disables the
//     hook with the empty name, which git 2.54 registers for a nameless [hook] section
//     (hook.event / hook.command) as well as for [hook ""] (git >= 2.55 ignores the nameless
//     form, and the flag is then an unused name); and hookOff carries
//     -c hook.<name>.enabled=false for every configured hook name plus
//     -c hook.<event>.enabled=false for every configured event (see configuredHookOverrides) —
//     git 2.54 has only the per-name switch. Together no hook runs, from either source.
//   - --no-optional-locks: status opportunistically refreshes and rewrites .git/index; skip it.
//     git diff does not honor this, which is why the session also points GIT_INDEX_FILE at a
//     throwaway copy of the index.
//
// Per-subcommand flags (--no-ext-diff, --no-textconv, --submodule=short) are added by the caller.
// Residuals (no git flag disables them; see doc.go):
//   - a repository-configured clean filter (filter.<name>.clean/process, e.g. git-lfs) selected by
//     .gitattributes still runs when git hashes a stat-dirty worktree file; overriding it would
//     make the reported delta wrong for those repositories.
//   - in a partial clone, a blob missing locally makes git lazily fetch it from the promisor
//     remote, which runs that remote's configured transport (e.g. core.sshCommand).
//     GIT_NO_LAZY_FETCH=1 (readOnlyGitEnv) disables this on git >= 2.44 only.
func readOnlyGitArgs(hooksDir string, hookOff, args []string) []string {
	pre := []string{
		"--no-pager",
		"-c", "core.fsmonitor=false",
		"-c", "core.useBuiltinFSMonitor=false",
		"-c", "core.hooksPath=" + hooksDir,
		"-c", "hook.post-index-change.enabled=false",
		"-c", "hook..enabled=false",
	}
	pre = append(pre, hookOff...)
	pre = append(pre, "--no-optional-locks")
	return append(pre, args...)
}

// hookConfigKeyPattern matches the keys that define a config hook: hook.<name>.command /
// hook.<name>.event (the name may be empty, [hook ""], or contain dots) and the two-part
// hook.command / hook.event of a nameless [hook] section, which git 2.54 registers under the
// empty name (git >= 2.55 ignores them).
const hookConfigKeyPattern = `^hook\.(.*\.)?(command|event)$`

// maxGitHookConfigOutput bounds the hook.* listing; a listing larger than this fails the session
// closed rather than running with hook names it could not read.
const maxGitHookConfigOutput = 64 << 10

// configuredHookOverrides lists every config-defined hook (git >= 2.54: hook.<name>.command /
// hook.<name>.event, and on git 2.54 also the nameless hook.command / hook.event, which define the
// hook named "", from every config scope git reads) and returns the -c flags that disable
// each one by name, plus each event those hooks name. The listing itself runs through the
// hardened session (git config runs no hooks). It is computed on every git version: on git < 2.54
// hook.* keys mean nothing, and the flags are ignored. Fails closed on a name that cannot be
// expressed as a -c key (one containing '=', which -c would split on).
func (g *readOnlyGit) configuredHookOverrides(ctx context.Context) ([]string, error) {
	out, truncated, err := g.run(ctx, maxGitHookConfigOutput, "config", "--null", "--get-regexp", hookConfigKeyPattern)
	if err != nil {
		// No matching key is a silent exit status 1: no configured hooks.
		var runErr *gitRunError
		if gitExitCode(err) == 1 && errors.As(err, &runErr) && runErr.stderr == "" {
			return nil, nil
		}
		return nil, fmt.Errorf("native: git: list configured hooks: %w", err)
	}
	if truncated {
		return nil, fmt.Errorf("native: git: list configured hooks: more than %d bytes of hook.* configuration", maxGitHookConfigOutput)
	}
	names, events, err := parseHookConfigListing(out)
	if err != nil {
		return nil, fmt.Errorf("native: git: %w", err)
	}
	flags := make([]string, 0, 2*(len(names)+len(events)))
	for _, n := range names {
		flags = append(flags, "-c", "hook."+n+".enabled=false")
	}
	for _, e := range events {
		flags = append(flags, "-c", "hook."+e+".enabled=false")
	}
	return flags, nil
}

// parseHookConfigListing parses `git config --null --get-regexp` output for hookConfigKeyPattern:
// NUL-terminated records of "key\nvalue" (or a bare "key" for a value-less entry). The key is
// hook.<name>.<variable> with section and variable lowercased and <name> (the subsection) kept
// verbatim, so <name> is everything between the first and the last dot and may itself contain
// dots or be empty (hook..event, from [hook ""]); git's -c parser splits a key the same way. The
// two-part hook.<variable> (a nameless [hook] section) is the empty name too: git 2.54 registers
// it as the hook named "", which -c hook..enabled=false disables. It returns the sorted, de-duplicated hook
// names and the sorted, de-duplicated event values (hook.<name>.event) that are expressible as a
// -c key. An event value containing '=' or a newline is not a real event and is skipped (the
// hook's name is disabled regardless); a name containing '=' is an error.
func parseHookConfigListing(out string) (names, events []string, err error) {
	nameSet, eventSet := map[string]bool{}, map[string]bool{}
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		key, value, _ := strings.Cut(rec, "\n")
		var name, variable string
		switch {
		case strings.HasSuffix(key, ".command"):
			name, variable = strings.TrimSuffix(key, ".command"), "command"
		case strings.HasSuffix(key, ".event"):
			name, variable = strings.TrimSuffix(key, ".event"), "event"
		default:
			return nil, nil, fmt.Errorf("unexpected hook config key %q", key)
		}
		rest, ok := strings.CutPrefix(name, "hook.")
		if name == "hook" { // hook.command / hook.event: the nameless [hook] section
			rest, ok = "", true
		}
		if !ok {
			return nil, nil, fmt.Errorf("unexpected hook config key %q", key)
		}
		if strings.Contains(rest, "=") {
			return nil, nil, fmt.Errorf("configured hook %q cannot be disabled on the command line (its name contains '='); refusing to run git with it enabled", rest)
		}
		nameSet[rest] = true
		if variable == "event" && value != "" && !strings.ContainsAny(value, "=\n") {
			eventSet[value] = true
		}
	}
	return sortedKeys(nameSet), sortedKeys(eventSet), nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// gitExitCode returns the exit status of a failed git run (errors from run/runGit wrap
// *exec.ExitError), or -1 when git did not exit normally (killed, timed out, not started).
func gitExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// readOnlyGitEnv is the process environment for a read-only git run: the ambient environment
// minus variables that name helpers to execute (GIT_EXTERNAL_DIFF, GIT_PAGER), plus
// GIT_OPTIONAL_LOCKS=0 (belt and braces with --no-optional-locks, and it also covers any git
// child process the command spawns, e.g. submodule status) and GIT_NO_LAZY_FETCH=1 (git >= 2.44:
// a partial clone fails on a missing blob instead of fetching it over the network).
func readOnlyGitEnv() []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "GIT_EXTERNAL_DIFF="),
			strings.HasPrefix(kv, "GIT_PAGER="),
			strings.HasPrefix(kv, "GIT_OPTIONAL_LOCKS="),
			strings.HasPrefix(kv, "GIT_NO_LAZY_FETCH="):
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1")
}

// readOnlyGit is one hardened inspection session over the workspace repository: a private temp
// directory holding an empty hooks directory (core.hooksPath, so no hook file is found), the -c
// flags that disable every config-defined hook (hookOff), and a throwaway copy of the index
// (GIT_INDEX_FILE, so a stat-cache refresh git decides to write never lands in .git/index). Every
// command goes through readOnlyGitArgs/readOnlyGitEnv. Call close when done.
type readOnlyGit struct {
	root     string
	hooksDir string
	hookOff  []string
	env      []string
	tmp      string
}

// newReadOnlyGit prepares a session. Failure is an error (fail closed): a read-only op must not fall
// back to running with the repository's hooks or against the live index. The configured hooks are
// listed first, before any command that could write an index. When the repository has no index
// yet there is nothing to isolate and GIT_INDEX_FILE is left unset.
func newReadOnlyGit(ctx context.Context, root string) (*readOnlyGit, error) {
	tmp, err := os.MkdirTemp("", "terfyn-git-ro-")
	if err != nil {
		return nil, fmt.Errorf("native: git: temp dir: %w", err)
	}
	g := &readOnlyGit{root: root, hooksDir: filepath.Join(tmp, "hooks"), env: readOnlyGitEnv(), tmp: tmp}
	if err := os.Mkdir(g.hooksDir, 0o700); err != nil {
		g.close()
		return nil, fmt.Errorf("native: git: hooks dir: %w", err)
	}
	if g.hookOff, err = g.configuredHookOverrides(ctx); err != nil {
		g.close()
		return nil, err
	}
	out, _, err := g.run(ctx, 4096, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		// --path-format needs git 2.31+; fall back to a cwd-relative path.
		if out, _, err = g.run(ctx, 4096, "rev-parse", "--git-path", "index"); err != nil {
			g.close()
			return nil, err
		}
	}
	src := strings.TrimSpace(out)
	if !filepath.IsAbs(src) {
		src = filepath.Join(root, src)
	}
	data, err := os.ReadFile(src)
	if errors.Is(err, os.ErrNotExist) {
		return g, nil
	}
	if err != nil {
		g.close()
		return nil, fmt.Errorf("native: git: read index: %w", err)
	}
	dst := filepath.Join(tmp, "index")
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		g.close()
		return nil, fmt.Errorf("native: git: index copy: %w", err)
	}
	g.env = append(g.env, "GIT_INDEX_FILE="+dst)
	return g, nil
}

func (g *readOnlyGit) close() { _ = os.RemoveAll(g.tmp) }

// gitWaitDelay bounds how long Run waits for the output copiers after git exits or the context is
// cancelled, so a grandchild that inherited the pipe cannot wedge the call.
const gitWaitDelay = 5 * time.Second

// run executes a read-only git command in the workspace root. stdout goes through a byte-capped
// writer and stderr through a separate bounded one, both assigned to cmd.Stdout/cmd.Stderr so
// os/exec owns the pipes and Run does not return until every byte git wrote has been copied
// (calling Wait before draining StdoutPipe can lose buffered tail data). The cap applies during
// I/O so a huge working-tree delta cannot be allocated before truncation (unlike CombinedOutput).
// truncated is true when stdout exceeded stdoutMax. stdin is empty. A non-zero exit is an error
// carrying a truncated tail of stderr (falling back to the capped stdout).
func (g *readOnlyGit) run(ctx context.Context, stdoutMax int, args ...string) (stdout string, truncated bool, err error) {
	cmd := exec.CommandContext(ctx, "git", readOnlyGitArgs(g.hooksDir, g.hookOff, args)...)
	cmd.Dir = g.root
	cmd.Env = g.env
	cmd.WaitDelay = gitWaitDelay
	outCap := &capBuffer{max: stdoutMax}
	errCap := &capBuffer{max: maxGitStderrBytes}
	cmd.Stdout = outCap
	cmd.Stderr = errCap
	if runErr := cmd.Run(); runErr != nil {
		return string(outCap.buf), outCap.truncated, &gitRunError{
			args:   args,
			err:    runErr,
			stderr: strings.TrimSpace(string(errCap.buf)),
			stdout: strings.TrimSpace(string(outCap.buf)),
		}
	}
	return string(outCap.buf), outCap.truncated, nil
}

// gitRunError is a failed readOnlyGit run. It unwraps to the *exec.ExitError (see gitExitCode) and
// keeps the bounded stderr apart, so a caller can tell a silent exit status (merge-base's "no
// common ancestor") from one git explained on stderr.
type gitRunError struct {
	args           []string
	err            error
	stderr, stdout string
}

func (e *gitRunError) Error() string {
	msg := e.stderr
	if msg == "" {
		msg = e.stdout
	}
	return fmt.Sprintf("native: git %s: %v: %s", strings.Join(e.args, " "), e.err, truncateRunes(msg, 512))
}

func (e *gitRunError) Unwrap() error { return e.err }

// maxGitOIDOutput bounds the stdout of an object-name lookup (rev-parse, merge-base, hash-object).
const maxGitOIDOutput = 4096

// commitOID resolves rev to a commit object name, or ok=false when it does not name a commit.
func (g *readOnlyGit) commitOID(ctx context.Context, rev string) (string, bool) {
	out, _, err := g.run(ctx, maxGitOIDOutput, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return "", false
	}
	oid := strings.TrimSpace(out)
	return oid, oid != ""
}

// headIsUnborn reports whether HEAD is a symbolic ref to a branch that has no commits yet (a
// fresh repository). A HEAD that is broken in any other way is not "unborn".
func (g *readOnlyGit) headIsUnborn(ctx context.Context) bool {
	if _, _, err := g.run(ctx, maxGitOIDOutput, "symbolic-ref", "--quiet", "HEAD"); err != nil {
		return false
	}
	_, _, err := g.run(ctx, maxGitOIDOutput, "rev-parse", "--verify", "--quiet", "HEAD")
	return err != nil
}

// emptyTreeOID computes the empty tree's object name in this repository's hash (SHA-1 or
// SHA-256) rather than hard-coding the SHA-1 value. hash-object without -w writes nothing, and
// --stdin without --path applies no filters.
func (g *readOnlyGit) emptyTreeOID(ctx context.Context) (string, error) {
	out, _, err := g.run(ctx, maxGitOIDOutput, "hash-object", "-t", "tree", "--stdin")
	if err != nil {
		return "", err
	}
	oid := strings.TrimSpace(out)
	if oid == "" {
		return "", fmt.Errorf("native: git: empty tree: no object name")
	}
	return oid, nil
}

// mergeBase returns the merge base of baseOID and head. Every failure is an error (fail closed —
// a two-dot fallback would diff against base's tip and misreport the change), but the error says
// which failure it is: git merge-base exits 1, silently, when it finds no common ancestor, and in
// a shallow clone that usually means the fork point was not fetched, not that the histories are
// unrelated. Any other failure (a timeout, a missing object) is reported as git's own error.
func (g *readOnlyGit) mergeBase(ctx context.Context, base, baseOID, head string) (string, error) {
	out, _, err := g.run(ctx, maxGitOIDOutput, "merge-base", baseOID, head)
	if err != nil {
		// No common ancestor is exit status 1 with nothing on stderr. Older git (2.34, for one) also exits 1
		// when the walk hits an unreadable commit, but then it says so on stderr.
		var runErr *gitRunError
		if gitExitCode(err) != 1 || !errors.As(err, &runErr) || runErr.stderr != "" {
			return "", fmt.Errorf("native: diff: merge base of base %q and HEAD: %w", base, err)
		}
		if shallow, _, serr := g.run(ctx, maxGitOIDOutput, "rev-parse", "--is-shallow-repository"); serr == nil && strings.TrimSpace(shallow) == "true" {
			return "", fmt.Errorf("native: diff: shallow clone: the fork point of HEAD and %q is not in the fetched history; fetch more history (git fetch --unshallow, or fetch-depth: 0 with actions/checkout) or omit base", base)
		}
		return "", fmt.Errorf("native: diff: base %q has no merge base with HEAD (unrelated histories)", base)
	}
	mb := strings.TrimSpace(out)
	if mb == "" {
		return "", fmt.Errorf("native: diff: merge base of base %q and HEAD: git printed no object name", base)
	}
	return mb, nil
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
		// Destructive cleanup must not run until the start point is a real commit. A typo like
		// base "maim" used to pass the syntactic name check, then `reset --hard` + `clean -fd`
		// deleted the workspace, and only then did `switch -C` fail. Resolve base first.
		if _, err := runGit(ctx, root, "rev-parse", "--verify", "--quiet", base+"^{commit}"); err != nil {
			return nil, fmt.Errorf("native: create_branch: base %q is not a commit", base)
		}
		if _, err := runGit(ctx, root, "check-ref-format", "--branch", name); err != nil {
			return nil, fmt.Errorf("native: create_branch: invalid branch name %q", name)
		}
		// Switch (discarding tracked dirt) before the broad untracked clean so a failed
		// transition cannot have already wiped leftover files.
		if _, err := runGit(ctx, root, "switch", "--discard-changes", "-C", name, base); err != nil {
			return nil, err
		}
		if _, err := runGit(ctx, root, "clean", "-fd"); err != nil {
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

// maxGitDiffBytes caps git.diff stdout during I/O the same way read_file caps its read (1 MiB
// of bytes) so a huge working tree degrades to truncated=true without first allocating the full
// CombinedOutput. maxGitStatusEntries caps git.status the same way glob caps match counts.
const (
	maxGitDiffBytes     = maxWorkspaceReadBytes
	maxGitStatusEntries = 1000
)

// gitDiff returns a unified diff of the workspace repository (issue #534). Every form compares
// against one explicitly resolved object, never a bare user string, and always ends the revision
// list with "--" so git cannot reinterpret anything as a pathspec:
//   - default: working tree vs HEAD (staged + unstaged tracked changes);
//   - staged:true: the index vs HEAD (git diff --cached);
//   - base: vs the merge base of base and HEAD (git merge-base base HEAD), i.e. what this branch
//     changed since it forked from base, as a pull request shows it — commits base gained after the
//     fork do not appear. base must resolve to a commit (checked like create_branch does) and the
//     merge base is echoed as merge_base. Combined with staged, it is the index vs that merge base.
//     No merge base is an error (see mergeBase: unrelated histories vs a shallow clone).
//   - unborn branch (no commits yet): HEAD is replaced by the empty tree so every tracked file
//     shows as added (unborn:true in the result); base is an error there (nothing to fork from).
//
// paths optionally scopes the pathspec. Untracked files do not appear in the diff — git.status
// lists those. The result is truncated like grep rather than unbounded.
func gitDiff(ctx context.Context, with map[string]any) (map[string]any, error) {
	root, err := workspaceRoot(ctx)
	if err != nil {
		return nil, err
	}
	paths, err := pathsFromWith(with, "paths")
	if err != nil {
		return nil, fmt.Errorf("native: diff: %w", err)
	}
	staged, _, err := optionalBoolFromWith(with, "staged")
	if err != nil {
		return nil, fmt.Errorf("native: diff: %w", err)
	}
	rawBase, _, err := optionalStringFromWith(with, "base")
	if err != nil {
		return nil, fmt.Errorf("native: diff: %w", err)
	}
	var base string
	if rawBase != "" {
		if base, err = validateBranchName("base", rawBase); err != nil {
			return nil, fmt.Errorf("native: diff: %w", err)
		}
	}

	g, err := newReadOnlyGit(ctx, root)
	if err != nil {
		return nil, err
	}
	defer g.close()

	result := map[string]any{}
	// from is the single object the working tree (or index) is compared against.
	var from string
	head, headOK := g.commitOID(ctx, "HEAD")
	if !headOK {
		if !g.headIsUnborn(ctx) {
			return nil, fmt.Errorf("native: diff: HEAD does not resolve to a commit")
		}
		if base != "" {
			return nil, fmt.Errorf("native: diff: base %q needs a commit on HEAD to find a merge base, but the current branch has no commits yet; omit base to diff against the empty tree", base)
		}
		if from, err = g.emptyTreeOID(ctx); err != nil {
			return nil, err
		}
		result["unborn"] = true
	}
	if base != "" {
		baseOID, ok := g.commitOID(ctx, base)
		if !ok {
			return nil, fmt.Errorf("native: diff: base %q is not a commit", base)
		}
		mb, err := g.mergeBase(ctx, base, baseOID, head)
		if err != nil {
			return nil, err
		}
		// Re-verify: the value handed to git diff must be a commit, not whatever merge-base printed.
		if from, ok = g.commitOID(ctx, mb); !ok {
			return nil, fmt.Errorf("native: diff: merge base %q of base %q is not a commit", mb, base)
		}
		result["base"] = base
		result["merge_base"] = from
	} else if headOK {
		from = head
	}

	// --submodule=short overrides a repository's diff.submodule=diff, which would otherwise spawn a
	// nested `git diff` inside each submodule that does not inherit --no-textconv/--no-ext-diff and
	// so runs the submodule's textconv driver or diff.external.
	args := []string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", "--submodule=short"}
	if staged {
		args = append(args, "--cached")
	}
	args = append(args, from, "--")
	args = append(args, paths...)
	diff, truncated, err := g.run(ctx, maxGitDiffBytes, args...)
	if err != nil {
		return nil, err
	}
	result["diff"] = diff
	result["truncated"] = truncated
	if staged {
		result["staged"] = true
	}
	return result, nil
}

// gitStatus lists changed/added/deleted/untracked paths in the workspace repository (issue #534).
// Porcelain v1 with -z so pathnames are unquoted NUL records (rename/copy is the two-path form)
// and a file named `a -> b` cannot be mistaken for a rename. paths optionally scopes the pathspec.
// The file list is capped like glob.
func gitStatus(ctx context.Context, with map[string]any) (map[string]any, error) {
	root, err := workspaceRoot(ctx)
	if err != nil {
		return nil, err
	}
	paths, err := pathsFromWith(with, "paths")
	if err != nil {
		return nil, fmt.Errorf("native: status: %w", err)
	}
	args := []string{"status", "--porcelain=v1", "-z", "--untracked-files=all"}
	if len(paths) > 0 {
		args = append(args, "--")
		args = append(args, paths...)
	}
	g, err := newReadOnlyGit(ctx, root)
	if err != nil {
		return nil, err
	}
	defer g.close()
	out, truncatedIO, err := g.run(ctx, maxGitDiffBytes, args...)
	if err != nil {
		return nil, err
	}
	files := parseGitStatusPorcelainZ(out)
	truncated := truncatedIO
	if len(files) > maxGitStatusEntries {
		files = files[:maxGitStatusEntries]
		truncated = true
	}
	pathList := make([]string, 0, len(files))
	for _, f := range files {
		if p, ok := f["path"].(string); ok && p != "" {
			pathList = append(pathList, p)
		}
	}
	return map[string]any{"files": files, "paths": pathList, "truncated": truncated}, nil
}

// parseGitStatusPorcelainZ parses `git status --porcelain=v1 -z`. Records are NUL-delimited and
// pathnames are unquoted, so a file literally named `a -> b` cannot be mistaken for a rename.
// Rename/copy uses git's two-path -z form: XY SP PATH NUL ORIG_PATH NUL (current path first,
// unlike the non-z "ORIG -> PATH" display).
func parseGitStatusPorcelainZ(out string) []map[string]any {
	b := []byte(out)
	files := make([]map[string]any, 0)
	i := 0
	for i < len(b) {
		if b[i] == 0 {
			i++
			continue
		}
		if i+3 > len(b) {
			break
		}
		index, worktree := string(b[i]), string(b[i+1])
		if b[i+2] != ' ' {
			// Malformed record: skip to the next NUL.
			for i < len(b) && b[i] != 0 {
				i++
			}
			if i < len(b) {
				i++
			}
			continue
		}
		i += 3
		first, ok := readNulField(b, &i)
		if !ok {
			break
		}
		path, from := first, ""
		if index == "R" || index == "C" || worktree == "R" || worktree == "C" {
			orig, ok2 := readNulField(b, &i)
			if !ok2 {
				break
			}
			from = orig
		}
		entry := map[string]any{
			"path":     path,
			"index":    index,
			"worktree": worktree,
			"status":   porcelainStatus(index, worktree),
		}
		if from != "" {
			entry["from"] = from
		}
		files = append(files, entry)
	}
	return files
}

func readNulField(b []byte, i *int) (string, bool) {
	if *i >= len(b) {
		return "", false
	}
	start := *i
	for *i < len(b) && b[*i] != 0 {
		*i++
	}
	if *i >= len(b) {
		// Truncated mid-field: drop the incomplete record.
		return "", false
	}
	s := string(b[start:*i])
	*i++ // consume NUL
	return s, true
}

func porcelainStatus(index, worktree string) string {
	switch {
	case index == "?" && worktree == "?":
		return "untracked"
	case index == "!" && worktree == "!":
		return "ignored"
	// Unmerged entries first: git's porcelain v1 marks a conflict with one of the XY pairs DD, AU,
	// UD, UA, DU, AA, UU, which would otherwise read as an ordinary add/delete/change.
	case index == "U" || worktree == "U",
		index == "D" && worktree == "D",
		index == "A" && worktree == "A":
		return "unmerged"
	case index == "A" || worktree == "A":
		return "added"
	case index == "D" || worktree == "D":
		return "deleted"
	case index == "R" || worktree == "R":
		return "renamed"
	case index == "C" || worktree == "C":
		return "copied"
	case index == "M" || worktree == "M":
		return "modified"
	default:
		return "changed"
	}
}

func dispatchGitDiff(ctx context.Context, with map[string]any, start time.Time) (map[string]any, ExecMeta, error) {
	out, err := gitDiff(ctx, with)
	meta := ExecMeta{DurationMs: time.Since(start).Milliseconds()}
	if err != nil {
		return nil, meta, err
	}
	return out, meta, nil
}

func dispatchGitStatus(ctx context.Context, with map[string]any, start time.Time) (map[string]any, ExecMeta, error) {
	out, err := gitStatus(ctx, with)
	meta := ExecMeta{DurationMs: time.Since(start).Milliseconds()}
	if err != nil {
		return nil, meta, err
	}
	return out, meta, nil
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
