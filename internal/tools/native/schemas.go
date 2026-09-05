package native

import "encoding/json"

// operationInputSchemas maps a native operation to the JSON Schema for its input.
// The agent tool-calling loop advertises these to the model so an agent knows a
// tool's required arguments — without them the model is handed an empty parameter
// schema and cannot pass e.g. `owner` to pull_request.get or `path` to read_file,
// so native tool calls fail on real providers (the mock model does not validate).
// Operations absent from this map advertise the permissive default (any object).
var operationInputSchemas = map[string]json.RawMessage{
	// workspace adapter
	"read_file":  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Workspace-relative path. A file returns its content; a directory (including \".\") returns its entries, so you can explore the tree."},"offset":{"type":"integer","minimum":1,"description":"Optional 1-based first line to return; with limit, reads only that span (e.g. grep hit at line 412 -> offset 380, limit 80) instead of the whole file. Omit both for the whole file."},"limit":{"type":"integer","minimum":1,"description":"Optional maximum number of lines to return from offset."}},"required":["path"],"additionalProperties":false}`),
	"write_file": json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Workspace-relative file path."},"content":{"type":"string","description":"Full new contents of the file."}},"required":["path","content"],"additionalProperties":false}`),
	"edit":       json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Workspace-relative file path."},"old_string":{"type":"string","description":"Exact text to replace. Must occur exactly once in the file; include enough surrounding context to be unique."},"new_string":{"type":"string","description":"Replacement text (may be empty to delete old_string)."}},"required":["path","old_string","new_string"],"additionalProperties":false}`),
	"run_tests":  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false,"description":"Runs the operator-configured test command; takes no arguments."}`),
	"list_dir":   json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Workspace-relative directory to list; omit or \".\" for the workspace root."}},"additionalProperties":false}`),
	"glob":       json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"Glob relative to the workspace root. ** matches across directories (e.g. \"**/*_test.go\"); * matches within one path segment (e.g. \"framework/*_test.go\")."}},"required":["pattern"],"additionalProperties":false}`),
	"grep":       json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"Go regexp (RE2) to search file contents for."},"path":{"type":"string","description":"Workspace-relative directory to search under; omit or \".\" for the whole workspace."}},"required":["pattern"],"additionalProperties":false}`),

	// github adapter
	"pull_request.get":          githubTripletSchema,
	"pull_request.diff":         githubTripletSchema,
	"pull_request.create":       json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"head":{"type":"string","description":"Branch with the change."},"base":{"type":"string","description":"Branch to merge into (e.g. main)."},"title":{"type":"string","description":"PR title (or set issue instead)."},"issue":{"type":"integer","description":"Issue number to convert into the PR, in place of title."},"body":{"type":"string"},"draft":{"type":"boolean"},"maintainer_can_modify":{"type":"boolean","description":"Allow maintainers to edit the PR branch."}},"required":["owner","repo","head","base"]}`),
	"pull_request.update":       json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"number":{"type":"integer"},"title":{"type":"string"},"body":{"type":"string"},"base":{"type":"string"},"state":{"type":"string","enum":["open","closed"]}},"required":["owner","repo","number"]}`),
	"pull_request.list":         json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"state":{"type":"string","enum":["open","closed","all"]},"head":{"type":"string"},"base":{"type":"string"}},"required":["owner","repo"]}`),
	"issues.get":                githubTripletSchema,
	"issues.update":             json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"number":{"type":"integer"},"title":{"type":"string"},"body":{"type":"string"},"state":{"type":"string","enum":["open","closed"]},"labels":{"type":"array","items":{"type":"string"}}},"required":["owner","repo","number"]}`),
	"issues.list":               json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"state":{"type":"string","enum":["open","closed","all"]},"labels":{"type":"string","description":"Comma-separated label names."}},"required":["owner","repo"]}`),
	"issues.comment":            json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"number":{"type":"integer"},"body":{"type":"string","description":"Comment body (Markdown)."}},"required":["owner","repo","number","body"]}`),
	"pull_request.post_comment": json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"number":{"type":"integer"},"body":{"type":"string","description":"Comment body (Markdown)."}},"required":["owner","repo","number","body"]}`),
	"check_runs.list":           json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"ref":{"type":"string","description":"Commit SHA or ref."}},"required":["owner","repo","ref"]}`),

	// git adapter
	"create_branch": json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Branch name. Idempotent: if it already exists the op switches to it (not an error)."},"reset":{"type":"boolean","description":"Force-recreate the branch at base, discarding a prior attempt's commits. Opt-in (destructive); requires base."},"base":{"type":"string","description":"Start point (ref/branch/sha) to create the branch from, e.g. \"main\". Required when reset is true; otherwise optional (defaults to the current HEAD)."}},"required":["name"],"additionalProperties":false}`),
	"push_branch":   json.RawMessage(`{"type":"object","properties":{"branch":{"type":"string","description":"Branch to push to the configured remote."}},"required":["branch"],"additionalProperties":false}`),
}

// githubTripletSchema is the owner/repo/number input shared by several github ops.
var githubTripletSchema = json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"number":{"type":"integer","description":"Issue or PR number."}},"required":["owner","repo","number"]}`)

// OperationInputSchema returns the input JSON Schema for a native operation and
// whether one is defined. A caller that gets ok=false should advertise a
// permissive default so the operation stays callable.
func OperationInputSchema(operation string) (json.RawMessage, bool) {
	s, ok := operationInputSchemas[operation]
	return s, ok
}
