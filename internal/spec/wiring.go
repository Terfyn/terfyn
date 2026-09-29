package spec

import (
	"regexp"
	"sort"
	"strings"

	"github.com/Terfyn/terfyn/internal/schema"
)

// interpTokenRE matches ${...} placeholders (design doc §13.1). Same shape as engine/interpolation.go.
var interpTokenRE = regexp.MustCompile(`\$\{([^}]*)\}`)

// validateStepWiring checks each agent step's `with` against the consumer agent's declared input
// schema on the graph (issue #193). Every with-key is a flow into the consumer at that key, and every
// flow goes through schema.CompatibleLookup, the same rule the .agent checker applies:
//
//   - a ${steps.*.output...} / ${input...} interpolation is typed by the producer's schema at that
//     path (an embedded token renders into a string);
//   - a with-key whose value contains no token at all — a string, number, bool, null, or an
//     object/array built only from those — is an untyped producer (schema.LookupResult{}), exactly
//     as the .agent checker types a literal argument (LitExpr / ObjectExpr are untyped). That is
//     gradual against every typed or untyped consumer, but still subject to the key being declared
//     and rejected by a never (false) consumer. A value that mixes literals and typed tokens is
//     checked through those tokens against the same consumer slot; a value whose only tokens this
//     pass does not type (${steps.<id>.status}, an unknown step, ...) is an untyped producer too,
//     so no with-key is left unchecked;
//   - a step with no `with` at all into a consumer whose whole input is never is rejected, matching
//     the .agent checker's zero-argument error — agent input is not validated at run time, so this
//     static check is what stops a false-input agent from running.
//
// Absent schemas are gradual.
func validateStepWiring(g *ProjectGraph) []error {
	if g == nil {
		return nil
	}
	var errs []error
	for wfName, wr := range g.Workflows {
		if wr == nil {
			continue
		}
		byID := workflowStepsByID(&wr.Spec)
		var inputDoc *schema.Document
		if wr.Spec.Input != nil {
			inputDoc = wr.Spec.Input.Resolved
		}
		for _, st := range wr.Spec.Steps {
			errs = append(errs, checkStepWithWiring(g, wfName, st, byID, inputDoc)...)
		}
	}
	return errs
}

func workflowStepsByID(w *WorkflowSpec) map[string]WorkflowStep {
	out := make(map[string]WorkflowStep, len(w.Steps))
	if w == nil {
		return out
	}
	for _, st := range w.Steps {
		id := strings.TrimSpace(st.ID)
		if id == "" {
			continue
		}
		out[id] = st
	}
	return out
}

func checkStepWithWiring(g *ProjectGraph, wfName string, st WorkflowStep, byID map[string]WorkflowStep, inputDoc *schema.Document) []error {
	// A synthetic (flattened control-flow) step is not an executable node: its `with`
	// carries structural placeholder keys (e.g. a single positional agent arg is
	// arg0, an agent input being a whole document, not named fields), and it is never
	// executed (#305, ADR 002 §5). Argument type safety is enforced by the checker's
	// type system; skip the per-field input-schema wiring check here.
	if st.Synthetic {
		return nil
	}
	consumer := consumerInputDoc(g, st)
	if len(st.With) == 0 {
		if consumer != nil && consumer.Lookup(nil).Impossible {
			return []error{st.Pos.Errorf(
				"workflow %s step %q: %s input schema is never (false) but the step supplies no with",
				wfName, strings.TrimSpace(st.ID), consumerSchemaName(st),
			)}
		}
		return nil
	}
	var errs []error
	// Sorted so diagnostics are deterministic regardless of map order.
	keys := make([]string, 0, len(st.With))
	for k := range st.With {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		// covered: some token in the value was typed and checked against the consumer slot.
		hasToken, covered := false, false
		walkWiringValue(st.With[key], func(s string) {
			tokErrs, tokCovered, tokFound := checkWiringString(g, wfName, st, key, s, byID, inputDoc, consumer)
			errs = append(errs, tokErrs...)
			covered = covered || tokCovered
			hasToken = hasToken || tokFound
		})
		if covered {
			continue
		}
		// No typed flow reached the consumer: a literal, or only tokens this pass does not type
		// (e.g. ${steps.<id>.status}, an unknown step). Either way the value is an untyped producer.
		what := "literal value"
		if hasToken {
			what = "untyped value"
		}
		errs = append(errs, checkConsumerType(wfName, st, key, what, schema.LookupResult{}, consumer, true)...)
	}
	return errs
}

func walkWiringValue(v any, fn func(string)) {
	switch t := v.(type) {
	case string:
		fn(t)
	case []any:
		for _, e := range t {
			walkWiringValue(e, fn)
		}
	case map[string]any:
		for _, e := range t {
			walkWiringValue(e, fn)
		}
	}
}

func checkWiringString(
	g *ProjectGraph,
	wfName string,
	st WorkflowStep,
	withKey, s string,
	byID map[string]WorkflowStep,
	inputDoc *schema.Document,
	consumer *schema.Document,
) (errs []error, covered, found bool) {
	tokens, whole := interpTokens(s)
	if len(tokens) == 0 {
		return nil, false, false
	}
	for _, inner := range tokens {
		e, c := checkInterpPath(g, wfName, st, withKey, inner, byID, inputDoc, consumer, whole && len(tokens) == 1)
		errs = append(errs, e...)
		covered = covered || c
	}
	return errs, covered, true
}

func interpTokens(s string) (inners []string, wholeField bool) {
	matches := interpTokenRE.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return nil, false
	}
	loc := interpTokenRE.FindStringIndex(s)
	wholeField = len(matches) == 1 && loc != nil && loc[0] == 0 && loc[1] == len(s)
	for _, m := range matches {
		inners = append(inners, strings.TrimSpace(m[1]))
	}
	return inners, wholeField
}

func checkInterpPath(
	g *ProjectGraph,
	wfName string,
	st WorkflowStep,
	withKey, inner string,
	byID map[string]WorkflowStep,
	inputDoc *schema.Document,
	consumer *schema.Document,
	wholeField bool,
) (errs []error, covered bool) {
	if inner == "" {
		return nil, false
	}
	parts := splitDotPath(inner)
	if len(parts) < 2 {
		return nil, false
	}
	switch parts[0] {
	case "input":
		return checkInputPath(wfName, st, withKey, inner, parts[1:], inputDoc, consumer, wholeField), true
	case "steps":
		if len(parts) < 3 {
			return nil, false
		}
		return checkStepsOutputPath(g, wfName, st, withKey, inner, parts, byID, consumer, wholeField)
	default:
		return nil, false
	}
}

func checkInputPath(
	wfName string,
	st WorkflowStep,
	withKey, inner string,
	tail []string,
	inputDoc *schema.Document,
	consumer *schema.Document,
	wholeField bool,
) []error {
	if inputDoc == nil {
		return checkConsumerType(wfName, st, withKey, "${"+inner+"}", schema.LookupResult{}, consumer, wholeField)
	}
	got := inputDoc.Lookup(tail)
	if got.Missing {
		return []error{st.Pos.Errorf(
			"workflow %s step %q: ${%s} is not declared in Workflow input schema",
			wfName, strings.TrimSpace(st.ID), inner,
		)}
	}
	return checkConsumerType(wfName, st, withKey, "${"+inner+"}", got, consumer, wholeField)
}

func checkStepsOutputPath(
	g *ProjectGraph,
	wfName string,
	st WorkflowStep,
	withKey, inner string,
	parts []string,
	byID map[string]WorkflowStep,
	consumer *schema.Document,
	wholeField bool,
) (errs []error, covered bool) {
	prodID := parts[1]
	slot := parts[2]
	if slot != "output" {
		return nil, false
	}
	prod, ok := byID[prodID]
	if !ok {
		return nil, false
	}
	doc := producerOutputDoc(g, prod)
	tail := parts[3:]
	var prodLookup schema.LookupResult
	if doc != nil {
		prodLookup = doc.Lookup(tail)
		if prodLookup.Missing {
			src := producerSchemaName(prod)
			return []error{st.Pos.Errorf(
				"workflow %s step %q: ${%s} is not declared in %s output schema",
				wfName, strings.TrimSpace(st.ID), inner, src,
			)}, true
		}
	}
	return checkConsumerType(wfName, st, withKey, "${"+inner+"}", prodLookup, consumer, wholeField), true
}

// checkConsumerType checks one producer flowing into the consumer's input at withKey. what names
// the producer in diagnostics: "${<path>}" for an interpolation token, "literal value" for a
// token-free with value.
func checkConsumerType(
	wfName string,
	st WorkflowStep,
	withKey, what string,
	prod schema.LookupResult,
	consumer *schema.Document,
	wholeField bool,
) []error {
	if consumer == nil || strings.TrimSpace(withKey) == "" {
		return nil
	}
	cons := consumer.Lookup([]string{withKey})
	if cons.Missing {
		return []error{st.Pos.Errorf(
			"workflow %s step %q: with %q is not declared in %s input schema",
			wfName, strings.TrimSpace(st.ID), withKey, consumerSchemaName(st),
		)}
	}
	// An embedded token is rendered into a string, so the producer the consumer sees is a string —
	// unless the producer is never: then the step cannot run and the rendered string never exists.
	src := prod
	if !wholeField && !prod.Impossible {
		src = schema.LookupResult{Types: schema.TypeSet{schema.TypeString: {}}, Known: true}
	}
	if schema.CompatibleLookup(src, cons) {
		return nil
	}
	return []error{st.Pos.Errorf(
		"workflow %s step %q: %s (%s) does not match %s input %q (%s)",
		wfName, strings.TrimSpace(st.ID), what, src, consumerSchemaName(st), withKey, cons,
	)}
}

func producerOutputDoc(g *ProjectGraph, st WorkflowStep) *schema.Document {
	name := strings.TrimSpace(st.Agent)
	if name == "" || g == nil {
		return nil
	}
	ar := g.Agents[name]
	if ar == nil || ar.Spec.Output == nil {
		return nil
	}
	return ar.Spec.Output.Resolved
}

func consumerInputDoc(g *ProjectGraph, st WorkflowStep) *schema.Document {
	name := strings.TrimSpace(st.Agent)
	if name == "" || g == nil {
		return nil
	}
	ar := g.Agents[name]
	if ar == nil || ar.Spec.Input == nil {
		return nil
	}
	return ar.Spec.Input.Resolved
}

func producerSchemaName(st WorkflowStep) string {
	name := strings.TrimSpace(st.Agent)
	if name == "" {
		return "producer"
	}
	return "Agent/" + name
}

func consumerSchemaName(st WorkflowStep) string {
	name := strings.TrimSpace(st.Agent)
	if name == "" {
		return "consumer"
	}
	return "Agent/" + name
}

func splitDotPath(path string) []string {
	var parts []string
	for _, p := range strings.Split(path, ".") {
		p = strings.TrimSpace(p)
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}
