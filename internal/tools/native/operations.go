package native

import (
	"sort"

	"github.com/Terfyn/terfyn/internal/spec"
)

// DispatchOperations lists operation names handled by [Registry.Dispatch] (excluding shell ops).
// Derived from dispatchHandlers; keep operationCatalog in sync (see TestRegistryDispatchMatchesCatalog).
var DispatchOperations = dispatchOperationNames()

func dispatchOperationNames() []string {
	names := make([]string, 0, len(dispatchHandlers))
	for name := range dispatchHandlers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// operationCatalog maps dispatch operation names to known top-level args (nil = arbitrary).
var operationCatalog = map[string][]string{
	"echo":               nil,
	"identity":           {"value"},
	"pull_request.fetch": {"pr"},
	"pull_request.post_comment": {
		"body", "owner", "repo", "number", "comment_id", "comment_strategy",
	},
	"pull_request.get":           {"owner", "repo", "number"},
	"pull_request.diff":          {"owner", "repo", "number"},
	"pull_request.create":        {"owner", "repo", "head", "base", "title", "issue", "body", "draft", "maintainer_can_modify"},
	"pull_request.update":        {"owner", "repo", "number", "title", "body", "base", "state"},
	"pull_request.list":          {"owner", "repo", "state", "head", "base"},
	"pull_request.create_review": {"owner", "repo", "number", "event", "body"},
	"check_runs.list":            {"owner", "repo", "ref"},
	"issues.create":              {"owner", "repo", "title", "body"},
	"issues.comment":             {"owner", "repo", "number", "body"},
	"issues.get":                 {"owner", "repo", "number"},
	"issues.update":              {"owner", "repo", "number", "title", "body", "state", "labels"},
	"issues.list":                {"owner", "repo", "state", "labels"},
	"commit_status.create":       {"owner", "repo", "sha", "state", "context", "description", "target_url"},
	"message.send":               {"channel", "text", "thread_ts"},
	"message.update":             {"channel", "ts", "text"},
	"create_branch":              {"name", "reset", "base"},
	"push_branch":                {"branch"},
	"read_file":                  {"path", "offset", "limit"},
	"write_file":                 {"path", "content"},
	"edit":                       {"path", "old_string", "new_string"},
	"run_tests":                  {}, // command comes from TERFYN_WORKSPACE_TEST_COMMAND, not tool args
	"list_dir":                   {"path"},
	"glob":                       {"pattern"},
	"grep":                       {"pattern", "path"},
}

// OperationKnown reports whether operation is implemented by [Registry.Dispatch].
func OperationKnown(operation string) bool {
	if _, ok := dispatchHandlers[operation]; ok {
		return true
	}
	return spec.IsShellCommandOperation(operation)
}

// DispatchOperationNames returns sorted dispatch operation names (excluding shell ops).
func DispatchOperationNames() []string {
	out := append([]string(nil), DispatchOperations...)
	sort.Strings(out)
	return out
}

// TopLevelArgsForOperation returns known top-level input keys for operation.
// The second value is false when the operation is unknown or accepts arbitrary keys (echo).
func TopLevelArgsForOperation(operation string) ([]string, bool) {
	if spec.IsShellCommandOperation(operation) {
		return []string{"command", "cmd", "script"}, true
	}
	args, ok := operationCatalog[operation]
	if !ok {
		return nil, false
	}
	if args == nil {
		return nil, true
	}
	return append([]string(nil), args...), true
}
