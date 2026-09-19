package lower

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/lang"
	"github.com/Terfyn/terfyn/internal/spec"
)

// LowerWorkflowResource lowers a YAML Workflow resource into the execution IR
// (issue #256, ADR 002 §5). It is the YAML-side counterpart to [LowerExec]: the
// two ingress paths must converge on one interpreter so execution semantics
// cannot diverge, and the differential bar is that a straight-line YAML workflow
// and its `.agent` twin lower to byte-identical [execir.Program]s (structure and
// digest).
//
// Mapping:
//
//   - A step's `uses:` → [execir.InvokeTool], `agent:` → [execir.InvokeAgent],
//     `workflow:` → [execir.InvokeWorkflow], `approval:` → [execir.Approval]
//     (the fourth XOR step kind, #195). The step id is the source binding name
//     the result is published under (its `Bind`), so a downstream
//     `${steps.<id>...}` resolves against it.
//   - A `with:` value is lowered by [lowerYAMLValue]: a whole-field `${...}`
//     token → [execir.Ref] in the SOURCE binding namespace (not the resource
//     interpolation token), a scalar → [execir.Lit], a sub-map/sequence →
//     [execir.Object]/[execir.List], and a string mixing prose with tokens →
//     [execir.Template]. Every `${...}` becomes a Ref in all four cases.
//   - `output.value` → a trailing [execir.Return]. The `.agent` convention
//     encodes a scalar `return X` as `output.value = {value: X}` (see
//     workflow.go lowerBody), so a single-key `{value: X}` is UNWRAPPED to
//     `Return X`; any other output map returns an [execir.Object].
//
// Concurrency (design decision 1, settled here per docs/plans/255): a workflow
// that opts into graph mode — any `needs:` key, [spec.WorkflowUsesExplicitNeeds]
// — lowers its steps into ONE [execir.Graph] node that preserves each step's
// authored dependency set, because YAML `needs:` is a general DAG that
// [execir.Fork] (series-parallel) cannot express faithfully. A straight-line
// (implicit-sequential) workflow instead lowers to a FLAT top-level node list,
// identical to the `.agent` straight-line twin.
//
// The reference namespace is the SOURCE binding namespace (step ids and the
// single conventional input parameter named `input`), never the resource-model
// `${steps.x.output}` token, so the execution IR is independent of how the
// resource projection renders interpolation. Diagnostics are lowering-time
// problems anchored at the owning step; a diagnostic-free Program is a valid
// executable projection. Execution of the Graph/Approval nodes is Phase 1/2
// (#257/#258) and out of scope here — this function only produces the program.
func LowerWorkflowResource(wf *spec.WorkflowResource) (*execir.Program, lang.Diagnostics) {
	if wf == nil {
		return &execir.Program{}, nil
	}
	wl := &wfResLowerer{wf: wf.Metadata.Name}
	prog := &execir.Program{
		Workflow: wf.Metadata.Name,
		// A YAML workflow has one conventional parameter, `input`, naming the
		// whole workflow input document — the root every `${input.*}` token
		// resolves against and the name the `.agent` twin gives its single
		// parameter, so the two lowerings share a binding namespace.
		Params: []string{"input"},
	}

	steps := wf.Spec.Steps
	if spec.WorkflowUsesExplicitNeeds(steps) {
		g := &execir.Graph{Pos: wf.Pos}
		for i := range steps {
			g.Nodes = append(g.Nodes, execir.GraphNode{
				ID:    strings.TrimSpace(steps[i].ID),
				Needs: spec.StepNeedsIDs(steps, i),
				Run:   wl.lowerStep(steps[i]),
			})
		}
		prog.Body = append(prog.Body, g)
	} else {
		for i := range steps {
			prog.Body = append(prog.Body, wl.lowerStep(steps[i]))
		}
	}

	if ret := wl.lowerOutput(wf.Spec.Output, wf.Pos); ret != nil {
		prog.Body = append(prog.Body, ret)
	}
	return prog, wl.diags
}

type wfResLowerer struct {
	wf    string
	diags lang.Diagnostics
}

func (wl *wfResLowerer) diag(p spec.Pos, format string, args ...any) {
	wl.diags = append(wl.diags, lang.Diagnostic{Pos: p, Msg: fmt.Sprintf(format, args...)})
}

// lowerStep classifies one step by its single set XOR field and lowers it. The
// step id is the binding name the result is published under.
func (wl *wfResLowerer) lowerStep(st spec.WorkflowStep) execir.Node {
	bind := strings.TrimSpace(st.ID)
	switch {
	case spec.StepIsApproval(st):
		return &execir.Approval{
			Pos:         st.Pos,
			Bind:        bind,
			Description: spec.ApprovalStepDescription(st),
			RedactKeys:  approvalRedactKeys(st),
			Args:        wl.lowerWith(st.With, st.Pos),
		}
	case strings.TrimSpace(st.Uses) != "":
		return &execir.InvokeTool{Pos: st.Pos, Bind: bind, Uses: st.Uses, Args: wl.lowerWith(st.With, st.Pos)}
	case strings.TrimSpace(st.Agent) != "":
		return &execir.InvokeAgent{Pos: st.Pos, Bind: bind, Agent: st.Agent, Args: wl.lowerWith(st.With, st.Pos)}
	case strings.TrimSpace(st.Workflow) != "":
		return &execir.InvokeWorkflow{Pos: st.Pos, Bind: bind, Workflow: st.Workflow, Args: wl.lowerWith(st.With, st.Pos)}
	default:
		wl.diag(st.Pos, "workflow %s step %q: has no uses, agent, workflow, or approval", wl.wf, bind)
		// A malformed step still yields a node so the program shape is stable; the
		// diagnostic is what callers must check.
		return &execir.InvokeAgent{Pos: st.Pos, Bind: bind}
	}
}

func approvalRedactKeys(st spec.WorkflowStep) []string {
	if st.Approval != nil && st.Approval.Config != nil && len(st.Approval.Config.RedactKeys) > 0 {
		out := make([]string, len(st.Approval.Config.RedactKeys))
		copy(out, st.Approval.Config.RedactKeys)
		return out
	}
	return nil
}

// lowerWith lowers a step's `with:` map into execir argument values. Keys are the
// argument names (matching the `.agent` named-argument convention).
func (wl *wfResLowerer) lowerWith(with map[string]any, pos spec.Pos) map[string]execir.Value {
	if len(with) == 0 {
		return nil
	}
	out := make(map[string]execir.Value, len(with))
	for k, v := range with {
		out[k] = wl.lowerYAMLValue(v, pos)
	}
	return out
}

// lowerOutput lowers `output.value` into a trailing Return, unwrapping the
// single-key `{value: X}` shape the `.agent` scalar-return convention produces so
// a YAML twin and its `.agent` twin agree.
func (wl *wfResLowerer) lowerOutput(out *spec.WorkflowOutput, pos spec.Pos) execir.Node {
	if out == nil || out.Value == nil {
		return nil
	}
	if inner, ok := singleValueField(out.Value); ok {
		return &execir.Return{Pos: pos, Value: wl.lowerYAMLValue(inner, pos)}
	}
	return &execir.Return{Pos: pos, Value: wl.lowerYAMLValue(out.Value, pos)}
}

// singleValueField reports the inner value X when m is exactly `{value: X}` — the
// canonical encoding of a scalar `return X` (workflow.go lowerBody) — so it
// unwraps to a bare Return value rather than a one-field Object.
func singleValueField(m map[string]any) (any, bool) {
	if len(m) != 1 {
		return nil, false
	}
	v, ok := m["value"]
	return v, ok
}

// lowerYAMLValue lowers one YAML value (as decoded into a with:/output tree) into
// an execir Value. A string is the only interpolation-bearing form: a whole-field
// token is a Ref, an embedded-token string is a Template, and a plain string is a
// Lit. Maps and sequences recurse.
func (wl *wfResLowerer) lowerYAMLValue(v any, pos spec.Pos) execir.Value {
	switch x := v.(type) {
	case string:
		return wl.lowerString(x, pos)
	case map[string]any:
		fields := make([]execir.Field, 0, len(x))
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic program shape; the digest sorts too
		for _, k := range keys {
			fields = append(fields, execir.Field{Key: k, Val: wl.lowerYAMLValue(x[k], pos)})
		}
		return execir.Object{Pos: pos, Fields: fields}
	case []any:
		elems := make([]execir.Value, len(x))
		for i := range x {
			elems[i] = wl.lowerYAMLValue(x[i], pos)
		}
		return execir.List{Pos: pos, Elems: elems}
	default:
		// A scalar leaf, canonicalized to execir's documented Lit types (string,
		// int64, float64, bool). This is load-bearing for twin-digest parity:
		// yaml.v3 decodes an authored integer as Go `int`, but the `.agent` parser
		// (parseNumber) produces `int64` for the same literal, and execir.litKey
		// tokenizes those two types differently — so an un-normalized `int` would
		// make a YAML workflow and its `.agent` twin hash differently for a
		// literal that executes identically. Widen numeric kinds here, at the
		// ingress boundary, rather than teaching every downstream Lit consumer
		// (digest, equality, marshalling) the yaml.v3 type split.
		return execir.Lit{Pos: pos, V: canonicalScalar(v)}
	}
}

// canonicalScalar widens a decoded YAML scalar to the Go type the `.agent` parser
// and execir use as canonical: any signed/unsigned integer kind → int64,
// float32 → float64; string, bool, float64, nil, and anything else pass through.
// A uint64 that does not fit int64 is left as-is (no `.agent` literal can reach
// that magnitude — parseNumber falls back to float64 past the int64 range).
func canonicalScalar(v any) any {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case uint:
		if uint64(x) <= math.MaxInt64 {
			return int64(x)
		}
	case uint8:
		return int64(x)
	case uint16:
		return int64(x)
	case uint32:
		return int64(x)
	case uint64:
		if x <= math.MaxInt64 {
			return int64(x)
		}
	case float32:
		return float64(x)
	}
	return v
}

var interpTokenRE = regexp.MustCompile(`\$\{([^}]*)\}`)

// lowerString lowers a YAML string leaf. It mirrors the engine's interpolation
// forms (§13.1): a whole-field token keeps the referent's native type (a Ref); a
// string with no token is a literal; a string mixing text and tokens is a
// Template whose token parts are Refs.
func (wl *wfResLowerer) lowerString(s string, pos spec.Pos) execir.Value {
	if inner, ok := wholeFieldToken(s); ok {
		return execir.Ref{Pos: pos, Path: wl.mapRefPath(inner, pos)}
	}
	locs := interpTokenRE.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return execir.Lit{Pos: pos, V: s}
	}
	var parts []execir.Value
	last := 0
	for _, loc := range locs {
		if loc[0] > last {
			parts = append(parts, execir.Lit{Pos: pos, V: s[last:loc[0]]})
		}
		inner := strings.TrimSpace(s[loc[2]:loc[3]])
		parts = append(parts, execir.Ref{Pos: pos, Path: wl.mapRefPath(inner, pos)})
		last = loc[1]
	}
	if last < len(s) {
		parts = append(parts, execir.Lit{Pos: pos, V: s[last:]})
	}
	return execir.Template{Pos: pos, Parts: parts}
}

// wholeFieldToken reports whether s is exactly one ${...} token (no surrounding
// text); it returns the trimmed inner path. Mirrors engine.wholeFieldToken.
func wholeFieldToken(s string) (string, bool) {
	loc := interpTokenRE.FindStringIndex(s)
	if loc == nil || loc[0] != 0 || loc[1] != len(s) {
		return "", false
	}
	m := interpTokenRE.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	return strings.TrimSpace(m[1]), true
}

// mapRefPath maps a resource-model interpolation path onto the source binding
// namespace an execir Ref uses:
//
//   - input.<field>...        → [input, field, …]   (the `input` parameter root)
//   - steps.<id>.output.<f>…  → [id, f, …]          (a step binds its output)
//   - steps.<id>.meta.<f>…    → [id, meta, f, …]    (no `.agent` twin; see note)
//
// The `.output` segment is dropped because a step's binding IS its output in the
// source namespace (the same way `x = tool()` binds x to the tool's output on the
// `.agent` side). `meta` has no `.agent` binding equivalent, so it is preserved
// as an explicit path segment; it never participates in the differential parity
// corpus. A malformed path is diagnosed and lowered best-effort.
func (wl *wfResLowerer) mapRefPath(inner string, pos spec.Pos) []string {
	parts := splitDotPath(inner)
	if len(parts) == 0 {
		wl.diag(pos, "workflow %s: empty interpolation ${}", wl.wf)
		return nil
	}
	switch parts[0] {
	case "input":
		// input.<field>... maps verbatim; a bare ${input} (whole input) has no
		// field to resolve and the resource model rejects it upstream, but it is
		// still lowered best-effort as [input].
		return parts
	case "steps":
		if len(parts) < 3 {
			wl.diag(pos, "workflow %s: interpolation %q must be steps.<id>.output|meta...", wl.wf, inner)
			return parts[1:]
		}
		id := parts[1]
		switch parts[2] {
		case "output":
			return append([]string{id}, parts[3:]...)
		case "meta":
			return append([]string{id, "meta"}, parts[3:]...)
		default:
			wl.diag(pos, "workflow %s: interpolation steps.%s must use .output or .meta, not %q", wl.wf, id, parts[2])
			return append([]string{id}, parts[2:]...)
		}
	default:
		wl.diag(pos, "workflow %s: interpolation %q must start with input or steps", wl.wf, inner)
		return parts
	}
}

// splitDotPath splits a dotted interpolation path, trimming whitespace and
// dropping empty segments (mirrors engine.splitPath).
func splitDotPath(path string) []string {
	var parts []string
	for _, p := range strings.Split(path, ".") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}
