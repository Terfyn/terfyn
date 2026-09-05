package native

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Terfyn/terfyn/internal/tools/toolerr"
)

// Workspace adapter (issue #323): a sandboxed filesystem + test-runner native tool.
//
// The sandbox root and the test command are config that lives outside the agent. They may be
// declared on the Tool resource (spec.workspace.root / testCommand, issue #323 follow-up) or, when
// not declared, come from the environment:
//   - TERFYN_WORKSPACE_ROOT         — the sandbox root; every read/write path resolves within it.
//   - TERFYN_WORKSPACE_TEST_COMMAND — the run_tests command, run via `sh -c` in the root.
//
// Declared config (carried on the context by the tools registry, which resolves a relative root
// against the project root) takes precedence over the env fallback. run_tests takes its command
// from config, NEVER from tool-call arguments, so a granted agent cannot choose an arbitrary
// command to execute — the capability boundary holds.
const (
	envWorkspaceRoot        = "TERFYN_WORKSPACE_ROOT"
	envWorkspaceTestCommand = "TERFYN_WORKSPACE_TEST_COMMAND"

	// maxWorkspaceReadBytes caps a single read_file result; maxWorkspaceTestOutputBytes caps
	// captured run_tests output. Both guard against an unbounded read into a tool result.
	maxWorkspaceReadBytes       = 1 << 20  // 1 MiB
	maxWorkspaceTestOutputBytes = 64 << 10 // 64 KiB
	// maxWorkspaceDirEntries caps a read_file directory listing the same way (a big
	// node_modules or generated tree degrades to truncated=true rather than unbounded).
	maxWorkspaceDirEntries = 1000
	// maxWorkspaceEditBytes caps the file an `edit` op will load and rewrite. A str_replace edit must
	// read the whole file, so a file larger than this is refused rather than loaded unbounded — the
	// same 1 MiB ceiling read_file uses for a whole-file read.
	maxWorkspaceEditBytes = maxWorkspaceReadBytes
)

// WorkspaceConfig is the declarative workspace config resolved from a Tool resource. Root is
// already absolute (the registry resolves a relative spec.workspace.root against the project root).
type WorkspaceConfig struct {
	Root        string
	TestCommand string
}

type workspaceConfigKey struct{}

// WithWorkspaceConfig carries a Tool's resolved workspace config on ctx so the native handlers use
// it in preference to the environment. The tools registry sets it per call when the Tool declares
// spec.workspace.
func WithWorkspaceConfig(ctx context.Context, cfg WorkspaceConfig) context.Context {
	return context.WithValue(ctx, workspaceConfigKey{}, cfg)
}

func workspaceConfigFromContext(ctx context.Context) WorkspaceConfig {
	if ctx == nil {
		return WorkspaceConfig{}
	}
	cfg, _ := ctx.Value(workspaceConfigKey{}).(WorkspaceConfig)
	return cfg
}

// workspaceRoot returns the absolute, existing sandbox root: the declared root on ctx if set,
// otherwise TERFYN_WORKSPACE_ROOT.
func workspaceRoot(ctx context.Context) (string, error) {
	root := strings.TrimSpace(workspaceConfigFromContext(ctx).Root)
	source := "spec.workspace.root"
	if root == "" {
		root = strings.TrimSpace(os.Getenv(envWorkspaceRoot))
		source = envWorkspaceRoot
	}
	if root == "" {
		return "", fmt.Errorf("native: no workspace root (set spec.workspace.root or %s)", envWorkspaceRoot)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("native: workspace root %q (%s): %w", root, source, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("native: workspace root %q (%s): %w", abs, source, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("native: workspace root %q (%s) is not a directory", abs, source)
	}
	return abs, nil
}

// workspaceTestCommand returns the run_tests command: the declared testCommand on ctx if set,
// otherwise TERFYN_WORKSPACE_TEST_COMMAND.
func workspaceTestCommand(ctx context.Context) (string, error) {
	cmd := strings.TrimSpace(workspaceConfigFromContext(ctx).TestCommand)
	if cmd == "" {
		cmd = strings.TrimSpace(os.Getenv(envWorkspaceTestCommand))
	}
	if cmd == "" {
		return "", fmt.Errorf("native: run_tests requires spec.workspace.testCommand or %s", envWorkspaceTestCommand)
	}
	return cmd, nil
}

// openWorkspaceRoot opens the sandbox root as an [os.Root]. Every read/write goes through the
// returned handle, whose methods resolve paths with openat semantics: a `..` component or a
// symlink that would leave the root is refused at the OS level (not by a string check), which
// also closes the check-then-open TOCTOU a lexical gate would leave. Callers must Close it.
func openWorkspaceRoot(ctx context.Context) (*os.Root, error) {
	dir, err := workspaceRoot(ctx)
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("native: workspace root %q: %w", dir, err)
	}
	return r, nil
}

// cleanWorkspaceRel normalizes a tool-supplied path to a forward-slash, root-relative name for
// os.Root. A leading slash is treated as sandbox-root-relative (not an absolute escape); os.Root
// enforces the real boundary on the remaining components, so `..`/symlink escapes are refused there.
func cleanWorkspaceRel(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", fmt.Errorf("field %q is required", "path")
	}
	rel = strings.TrimLeft(filepath.ToSlash(rel), "/")
	if rel == "" {
		return "", fmt.Errorf("field %q is required", "path")
	}
	return rel, nil
}

// readWorkspaceDirEntries reads a directory handle's entries as sorted names (sub-directories
// suffixed "/"), bounded at maxWorkspaceDirEntries so a pathological tree (a big node_modules, a
// generated tree) degrades to truncated=true rather than an unbounded read + oversized tool result.
// Shared by read_file's directory branch and list_dir so a cap change lands in one place.
func readWorkspaceDirEntries(f *os.File) (names []string, truncated bool, err error) {
	ents, rderr := f.ReadDir(maxWorkspaceDirEntries + 1)
	if rderr != nil && !errors.Is(rderr, io.EOF) {
		return nil, false, rderr
	}
	if len(ents) > maxWorkspaceDirEntries {
		ents = ents[:maxWorkspaceDirEntries]
		truncated = true
	}
	names = make([]string, 0, len(ents))
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, truncated, nil
}

// classifyWorkspacePathErr turns a filesystem error from a workspace path op into either a
// recoverable observation the agent can act on or a plain (fatal-by-default) error. Only a genuine
// MISS — the path does not exist (fs.ErrNotExist) — is recoverable, so `read_file` on a guessed
// path that isn't there lets the agent try another path instead of killing the run (issue #451). A
// sandbox-escape rejection from os.Root is NOT fs.ErrNotExist, and a permission error the agent
// cannot fix is not a miss, so both stay fatal. The observation echoes only rel — the agent's own
// input — never the underlying OS error text, which the fatal/trace path keeps for the operator.
func classifyWorkspacePathErr(op, rel string, err error) error {
	full := fmt.Errorf("native: %s %q: %w", op, rel, err)
	if errors.Is(err, fs.ErrNotExist) {
		return toolerr.Recoverable(fmt.Sprintf("%s: %q does not exist in the workspace", op, rel), full)
	}
	return full
}

func dispatchWorkspaceReadFile(ctx context.Context, with map[string]any, start time.Time) (map[string]any, ExecMeta, error) {
	meta := ExecMeta{DurationMs: time.Since(start).Milliseconds()}
	root, err := openWorkspaceRoot(ctx)
	if err != nil {
		return nil, meta, err
	}
	defer root.Close()
	rawPath, err := stringFromWith(with, "path")
	if err != nil {
		return nil, meta, fmt.Errorf("native: read_file: %w", err)
	}
	rel, err := cleanWorkspaceRel(rawPath)
	if err != nil {
		return nil, meta, fmt.Errorf("native: read_file: %w", err)
	}
	offset, limit, err := readFileRangeArgs(with)
	if err != nil {
		return nil, meta, fmt.Errorf("native: read_file: %w", err)
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, meta, classifyWorkspacePathErr("read_file", rel, err)
	}
	defer f.Close()
	// A directory is not an error: return its entries so an agent can explore the tree even via
	// read_file (list_dir is the dedicated op; this branch shares readWorkspaceDirEntries with it).
	// Sub-directories are marked with a trailing "/". offset/limit are line bounds for a file, so a
	// directory ignores them.
	if info, statErr := f.Stat(); statErr == nil && info.IsDir() {
		names, truncated, rderr := readWorkspaceDirEntries(f)
		if rderr != nil {
			return nil, meta, fmt.Errorf("native: read_file %q (directory): %w", rawPath, rderr)
		}
		meta.DurationMs = time.Since(start).Milliseconds()
		out := map[string]any{"path": rel, "is_directory": true, "entries": names}
		if truncated {
			out["truncated"] = true
		}
		return out, meta, nil
	}
	// A line range (offset/limit) returns only the requested span so an agent that grep'd a hit at
	// foo.go:412 can read lines 380-460 instead of the whole file (issue #512). The scan is bounded and
	// cancellable exactly like read_file's whole-file branch and grep: it reads in fixed-size chunks so
	// a giant/minified line is never loaded whole, caps the RETURNED content at maxWorkspaceReadBytes,
	// and honors ctx on every iteration. Absent offset/limit preserves today's whole-file behavior below.
	if offset > 0 || limit > 0 {
		out, rderr := readWorkspaceLineRange(ctx, f, rel, offset, limit)
		if rderr != nil {
			return nil, meta, fmt.Errorf("native: read_file %q: %w", rawPath, rderr)
		}
		meta.DurationMs = time.Since(start).Milliseconds()
		return out, meta, nil
	}
	// Bound the read itself, not just the result: read at most one byte past the cap so a larger
	// file is reported truncated without loading all of it into memory.
	data, err := io.ReadAll(io.LimitReader(f, maxWorkspaceReadBytes+1))
	if err != nil {
		return nil, meta, fmt.Errorf("native: read_file %q: %w", rawPath, err)
	}
	truncated := false
	if len(data) > maxWorkspaceReadBytes {
		data = data[:maxWorkspaceReadBytes]
		truncated = true
	}
	out := map[string]any{
		"path":    rel,
		"content": string(data),
		"bytes":   len(data),
	}
	if truncated {
		out["truncated"] = true
	}
	meta.DurationMs = time.Since(start).Milliseconds()
	return out, meta, nil
}

// readFileRangeArgs reads read_file's optional line-range args. offset is a 1-based starting line
// (0 = unset, meaning from the first line); limit is a max line count (0 = unset, meaning to EOF).
// A present-but-out-of-range value (offset < 1, limit < 1) is a bad-input error rather than a silent
// clamp, so a mistyped bound is reported instead of quietly returning the wrong span.
func readFileRangeArgs(with map[string]any) (offset, limit int, err error) {
	offset, _, err = optionalIntFromWith(with, "offset")
	if err != nil {
		return 0, 0, err
	}
	if offset < 0 || (hasKey(with, "offset") && offset < 1) {
		return 0, 0, fmt.Errorf("field %q must be a line number >= 1", "offset")
	}
	limit, _, err = optionalIntFromWith(with, "limit")
	if err != nil {
		return 0, 0, err
	}
	if limit < 0 || (hasKey(with, "limit") && limit < 1) {
		return 0, 0, fmt.Errorf("field %q must be a line count >= 1", "limit")
	}
	return offset, limit, nil
}

// lineRangeScanBufBytes is the fixed working buffer for a range read: ReadSlice returns at most this
// many bytes per call, so a giant/minified line is consumed in bounded chunks instead of one alloc.
const lineRangeScanBufBytes = 64 << 10

// readWorkspaceLineRange returns the [offset, offset+limit) line span of f (offset 1-based; offset 0
// means from line 1; limit 0 means to EOF). It is bounded and cancellable like the whole-file branch:
// lines are read in fixed-size chunks (never a single unbounded alloc for a giant line), bytes before
// offset are discarded rather than retained, the RETURNED content is capped at maxWorkspaceReadBytes
// (exceeding it sets truncated=true), and ctx is checked on every iteration. start_line/end_line
// report the span returned (end_line < start_line when the offset is past EOF, i.e. no lines matched).
func readWorkspaceLineRange(ctx context.Context, f fs.File, rel string, offset, limit int) (map[string]any, error) {
	if offset < 1 {
		offset = 1
	}
	br := bufio.NewReaderSize(f, lineRangeScanBufBytes)
	var b strings.Builder
	lineNo := 1
	returned := 0
	truncated := false
	// wroteCurrentLine tracks whether any byte of the line now being read reached content, so returned
	// counts lines that actually CONTRIBUTED bytes — not loop iterations. A line whose first collected
	// fragment hits the byte cap (room<=0, nothing written) must not count; a truncated PREFIX of a
	// line must count so end_line names it. Otherwise lines/end_line would misdescribe content and
	// break offset=end_line+1 pagination (issue #512 review).
	wroteCurrentLine := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// ReadSlice returns a fragment of the current line: nil err at the newline, io.EOF for a final
		// line with no newline, or ErrBufferFull mid-line (the line is longer than the buffer). A
		// trailing empty read at a clean EOF (file ends in "\n") is not a line.
		frag, rderr := br.ReadSlice('\n')
		if rderr == io.EOF && len(frag) == 0 {
			break
		}
		if rderr != nil && rderr != bufio.ErrBufferFull && rderr != io.EOF {
			return nil, rderr
		}
		collecting := lineNo >= offset && (limit == 0 || returned < limit)
		if collecting && !truncated {
			switch room := maxWorkspaceReadBytes - b.Len(); {
			case room <= 0:
				truncated = true
			case len(frag) > room:
				b.Write(frag[:room])
				wroteCurrentLine = true
				truncated = true
			case len(frag) > 0:
				b.Write(frag)
				wroteCurrentLine = true
			}
		}
		lineComplete := rderr != bufio.ErrBufferFull // nil (newline) or io.EOF
		if lineComplete {
			if wroteCurrentLine {
				returned++
			}
			lineNo++
			wroteCurrentLine = false
		}
		if truncated {
			// A mid-line truncation (line did not complete) still contributed a prefix: count it so
			// end_line names that line. A line that completed already counted above (wroteCurrentLine
			// is reset), and a room<=0 line that wrote nothing correctly does not count.
			if wroteCurrentLine {
				returned++
			}
			break
		}
		if rderr == io.EOF {
			break
		}
		if lineComplete && limit > 0 && returned >= limit {
			break
		}
	}
	out := map[string]any{
		"path":       rel,
		"content":    b.String(),
		"bytes":      b.Len(),
		"start_line": offset,
		"end_line":   offset + returned - 1,
		"lines":      returned,
	}
	if truncated {
		out["truncated"] = true
	}
	return out, nil
}

func dispatchWorkspaceWriteFile(ctx context.Context, with map[string]any, start time.Time) (map[string]any, ExecMeta, error) {
	meta := ExecMeta{DurationMs: time.Since(start).Milliseconds()}
	root, err := openWorkspaceRoot(ctx)
	if err != nil {
		return nil, meta, err
	}
	defer root.Close()
	rawPath, err := stringFromWith(with, "path")
	if err != nil {
		return nil, meta, fmt.Errorf("native: write_file: %w", err)
	}
	content, err := contentFromWith(with)
	if err != nil {
		return nil, meta, fmt.Errorf("native: write_file: %w", err)
	}
	rel, err := cleanWorkspaceRel(rawPath)
	if err != nil {
		return nil, meta, fmt.Errorf("native: write_file: %w", err)
	}
	if dir := path.Dir(rel); dir != "." {
		// MkdirAll resolves through os.Root too, so a parent that escapes via `..`/symlink is refused.
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return nil, meta, fmt.Errorf("native: write_file %q: %w", rawPath, err)
		}
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, meta, fmt.Errorf("native: write_file %q: %w", rawPath, err)
	}
	_, writeErr := f.Write([]byte(content))
	closeErr := f.Close()
	if writeErr != nil {
		return nil, meta, fmt.Errorf("native: write_file %q: %w", rawPath, writeErr)
	}
	if closeErr != nil {
		return nil, meta, fmt.Errorf("native: write_file %q: %w", rawPath, closeErr)
	}
	meta.DurationMs = time.Since(start).Milliseconds()
	return map[string]any{"path": rel, "bytes": len(content), "ok": true}, meta, nil
}

// dispatchWorkspaceEdit performs a surgical, in-place edit: it replaces the single exact occurrence
// of old_string with new_string (issue #512). Unlike write_file — which rewrites the whole file, so
// changing three lines means the model must reproduce the entire file — an edit lets an agent alter a
// span by quoting only it. old_string must match EXACTLY ONCE: zero matches is an error the agent can
// correct (re-read and re-quote), and two-or-more is an error asking for more surrounding context, so
// an edit never silently changes the wrong occurrence. It carries workspace.write, exactly like
// write_file; granting stays per-op, so an author can hand out the range read without the edit.
func dispatchWorkspaceEdit(ctx context.Context, with map[string]any, start time.Time) (map[string]any, ExecMeta, error) {
	meta := ExecMeta{DurationMs: time.Since(start).Milliseconds()}
	root, err := openWorkspaceRoot(ctx)
	if err != nil {
		return nil, meta, err
	}
	defer root.Close()
	rawPath, err := stringFromWith(with, "path")
	if err != nil {
		return nil, meta, fmt.Errorf("native: edit: %w", err)
	}
	oldStr, err := requiredEditString(with, "old_string")
	if err != nil {
		return nil, meta, fmt.Errorf("native: edit: %w", err)
	}
	newStr, err := requiredEditString(with, "new_string")
	if err != nil {
		return nil, meta, fmt.Errorf("native: edit: %w", err)
	}
	if oldStr == "" {
		return nil, meta, fmt.Errorf("native: edit: field %q must not be empty (it locates the text to replace)", "old_string")
	}
	if oldStr == newStr {
		return nil, meta, fmt.Errorf("native: edit: old_string and new_string are identical; the edit would change nothing")
	}
	rel, err := cleanWorkspaceRel(rawPath)
	if err != nil {
		return nil, meta, fmt.Errorf("native: edit: %w", err)
	}

	f, err := root.Open(rel)
	if err != nil {
		return nil, meta, classifyWorkspacePathErr("edit", rel, err)
	}
	info, statErr := f.Stat()
	if statErr != nil {
		f.Close()
		return nil, meta, fmt.Errorf("native: edit %q: %w", rawPath, statErr)
	}
	if info.IsDir() {
		f.Close()
		return nil, meta, fmt.Errorf("native: edit %q: is a directory", rawPath)
	}
	// A str_replace must read the whole file; refuse one past the cap rather than load it unbounded.
	data, err := io.ReadAll(io.LimitReader(f, maxWorkspaceEditBytes+1))
	f.Close()
	if err != nil {
		return nil, meta, fmt.Errorf("native: edit %q: %w", rawPath, err)
	}
	if len(data) > maxWorkspaceEditBytes {
		return nil, meta, fmt.Errorf("native: edit %q: file exceeds %d bytes; edit a smaller file or use write_file", rawPath, maxWorkspaceEditBytes)
	}

	content := string(data)
	switch n := strings.Count(content, oldStr); {
	case n == 0:
		// Recoverable: the agent can re-read and re-quote rather than the run dying (issue #451).
		full := fmt.Errorf("native: edit %q: old_string not found", rawPath)
		return nil, meta, toolerr.Recoverable(fmt.Sprintf("edit: old_string not found in %q; re-read the file and quote the exact current text", rel), full)
	case n > 1:
		full := fmt.Errorf("native: edit %q: old_string matches %d times", rawPath, n)
		return nil, meta, toolerr.Recoverable(fmt.Sprintf("edit: old_string matches %d times in %q; include more surrounding context so it is unique", n, rel), full)
	}
	updated := strings.Replace(content, oldStr, newStr, 1)

	// Write atomically: a fresh sibling temp, then Rename over the target. Unlike write_file, edit's
	// replacement bytes are NOT in the tool args, so truncating the original in place (O_TRUNC) would
	// make a failed/short write unrecoverable data loss. The original inode is untouched until the new
	// bytes are durably closed and the rename swaps it in; any failure leaves the file exactly as it
	// was (issue #512 review). Rename resolves through os.Root, so it cannot escape the sandbox.
	if err := writeFileAtomically(root, rel, []byte(updated), info.Mode().Perm()); err != nil {
		return nil, meta, fmt.Errorf("native: edit %q: %w", rawPath, err)
	}
	meta.DurationMs = time.Since(start).Milliseconds()
	return map[string]any{"path": rel, "bytes": len(updated), "replaced": 1, "ok": true}, meta, nil
}

// writeFileAtomically replaces rel's contents with data without ever truncating rel in place: it
// writes a new O_EXCL sibling temp in the same directory (so the Rename is a same-filesystem atomic
// swap), fsyncs and closes it, then renames it over rel. On any error the temp is removed and rel is
// left untouched. perm is applied to the replacement so an executable/mode is preserved. All paths
// resolve through os.Root, so neither the temp nor the rename can escape the sandbox.
func writeFileAtomically(root *os.Root, rel string, data []byte, perm os.FileMode) error {
	dir := path.Dir(rel)
	tmpRel := path.Join(dir, fmt.Sprintf(".%s.terfyn-edit-%d.tmp", path.Base(rel), time.Now().UnixNano()))
	tf, err := root.OpenFile(tmpRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := tf.Write(data); err != nil {
		tf.Close()
		_ = root.Remove(tmpRel)
		return err
	}
	if err := tf.Sync(); err != nil {
		tf.Close()
		_ = root.Remove(tmpRel)
		return err
	}
	if err := tf.Close(); err != nil {
		_ = root.Remove(tmpRel)
		return err
	}
	// perm was requested at create time but is subject to umask; set it explicitly so the replacement
	// keeps the original file's mode.
	if err := root.Chmod(tmpRel, perm); err != nil {
		_ = root.Remove(tmpRel)
		return err
	}
	if err := root.Rename(tmpRel, rel); err != nil {
		_ = root.Remove(tmpRel)
		return err
	}
	return nil
}

// requiredEditString reads a required string arg for edit. An explicitly empty string is valid for
// new_string (a deletion); the caller enforces old_string non-emptiness separately. An absent field
// is an error.
func requiredEditString(with map[string]any, key string) (string, error) {
	v, ok := with[key]
	if !ok || v == nil {
		return "", fmt.Errorf("field %q is required", key)
	}
	s, err := scalarToString(v)
	if err != nil {
		return "", fmt.Errorf("field %q: %w", key, err)
	}
	return s, nil
}

// hasKey reports whether with carries key with a non-nil value.
func hasKey(with map[string]any, key string) bool {
	v, ok := with[key]
	return ok && v != nil
}

// optionalIntFromWith reads an optional integer arg. Absent/nil returns (0, false, nil). A present
// value may arrive as a JSON number (float64/json.Number over the wire), a Go int, or a numeric
// string; a non-integer or non-numeric value is a bad-input error, not a silent zero.
func optionalIntFromWith(with map[string]any, key string) (int, bool, error) {
	v, ok := with[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	switch x := v.(type) {
	case int:
		return x, true, nil
	case int64:
		return int(x), true, nil
	case float64:
		if x != float64(int64(x)) {
			return 0, false, fmt.Errorf("field %q must be an integer, got %v", key, x)
		}
		return int(x), true, nil
	case json.Number:
		n, err := x.Int64()
		if err != nil {
			return 0, false, fmt.Errorf("field %q must be an integer: %w", key, err)
		}
		return int(n), true, nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil {
			return 0, false, fmt.Errorf("field %q must be an integer: %w", key, err)
		}
		return n, true, nil
	default:
		return 0, false, fmt.Errorf("field %q must be an integer, got %T", key, v)
	}
}

// optionalBoolFromWith reads an optional boolean arg. Absent/nil returns (false, false, nil). A
// present value may be a Go bool or the string "true"/"false"; any other value is a bad-input error.
func optionalBoolFromWith(with map[string]any, key string) (bool, bool, error) {
	v, ok := with[key]
	if !ok || v == nil {
		return false, false, nil
	}
	switch x := v.(type) {
	case bool:
		return x, true, nil
	case string:
		switch strings.TrimSpace(strings.ToLower(x)) {
		case "true":
			return true, true, nil
		case "false":
			return false, true, nil
		}
	}
	return false, false, fmt.Errorf("field %q must be a boolean, got %T", key, v)
}

// contentFromWith reads the required string `content` arg. An explicitly empty string is a valid
// write (truncate to empty); only an absent content field is an error.
func contentFromWith(with map[string]any) (string, error) {
	v, ok := with["content"]
	if !ok || v == nil {
		return "", fmt.Errorf("field %q is required", "content")
	}
	s, err := scalarToString(v)
	if err != nil {
		return "", fmt.Errorf("field %q: %w", "content", err)
	}
	return s, nil
}

func dispatchWorkspaceRunTests(ctx context.Context, _ map[string]any, start time.Time) (map[string]any, ExecMeta, error) {
	meta := ExecMeta{DurationMs: time.Since(start).Milliseconds()}
	root, err := workspaceRoot(ctx)
	if err != nil {
		return nil, meta, err
	}
	command, err := workspaceTestCommand(ctx)
	if err != nil {
		return nil, meta, err
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = root
	combined, runErr := cmd.CombinedOutput()

	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			// The command could not be started or was killed (e.g. context cancel/timeout):
			// that is a genuine tool failure, not a test result.
			return nil, meta, fmt.Errorf("native: run_tests %q: %w", command, runErr)
		}
	}

	output := string(combined)
	truncated := false
	if len(output) > maxWorkspaceTestOutputBytes {
		output = output[:maxWorkspaceTestOutputBytes]
		truncated = true
	}
	out := map[string]any{
		"command":  command,
		"exitCode": exitCode,
		"passed":   exitCode == 0,
		"output":   output,
	}
	if truncated {
		out["truncated"] = true
	}
	meta.DurationMs = time.Since(start).Milliseconds()
	return out, meta, nil
}
