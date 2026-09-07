package models

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Terfyn/terfyn/internal/models/anthropic"
)

// maxDiagTailBlocks bounds how many trailing content blocks the 4xx diagnostic names, so the context
// stays a short line rather than a transcript.
const maxDiagTailBlocks = 8

// diagnoseAnthropicRequest renders redacted, structural context about a request the provider rejected
// with a 4xx (issue #524). A bare "Invalid request data" with no request body is undebuggable, so this
// attaches what can be shown safely: the message count, the trailing block types, and the
// tool_use/tool_result pairing. It carries NO content — only counts, roles, block-type names, and
// provider-generated tool ids (opaque tokens, e.g. `toolu_…`, not secrets) — so it is safe on an error
// string and in a trace.
func diagnoseAnthropicRequest(msgs []anthropic.ChatMessage) string {
	toolUseIDs := make(map[string]bool)
	toolResultIDs := make(map[string]bool)
	var tail []string
	for _, m := range msgs {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if len(m.Blocks) == 0 {
			tail = append(tail, role+":text")
			continue
		}
		for _, b := range m.Blocks {
			tail = append(tail, role+":"+b.Type)
			switch b.Type {
			case "tool_use":
				if id := strings.TrimSpace(b.ID); id != "" {
					toolUseIDs[id] = true
				}
			case "tool_result":
				if id := strings.TrimSpace(b.ToolUseID); id != "" {
					toolResultIDs[id] = true
				}
			}
		}
	}
	if len(tail) > maxDiagTailBlocks {
		tail = tail[len(tail)-maxDiagTailBlocks:]
	}

	var unanswered []string
	for id := range toolUseIDs {
		if !toolResultIDs[id] {
			unanswered = append(unanswered, id)
		}
	}
	sort.Strings(unanswered)

	return fmt.Sprintf("messages=%d; tail=[%s]; tool_use=%d tool_result=%d unanswered=[%s]",
		len(msgs), strings.Join(tail, " "), len(toolUseIDs), len(toolResultIDs), strings.Join(unanswered, " "))
}
