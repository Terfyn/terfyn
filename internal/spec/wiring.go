package spec

import (
	"regexp"
	"strings"

	"github.com/Terfyn/terfyn/internal/schema"
)

// interpTokenRE matches ${...} placeholders (design doc §13.1). Same shape as engine/interpolation.go.
var interpTokenRE = regexp.MustCompile(`\$\{([^}]*)\}`)

// validateStepWiring checks ${steps.*.output...} (and ${input...}) interpolations against
// declared schemas on the graph (issue #193). Absent schemas are skipped (gradual typing).
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
	if len(st.With) == 0 {
		return nil
	}
	consumer := consumerInputDoc(g, st)
	var errs []error
	for key, val := range st.With {
		walkWiringValue(val, func(s string) {
			errs = append(errs, checkWiringString(g, wfName, st, key, s, byID, inputDoc, consumer)...)
		})
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
) []error {
	tokens, whole := interpTokens(s)
	if len(tokens) == 0 {
		return nil
	}
	var errs []error
	for _, inner := range tokens {
		errs = append(errs, checkInterpPath(g, wfName, st, withKey, inner, byID, inputDoc, consumer, whole && len(tokens) == 1)...)
	}
	return errs
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
) []error {
	if inner == "" {
		return nil
	}
	parts := splitDotPath(inner)
	if len(parts) < 2 {
		return nil
	}
	switch parts[0] {
	case "input":
		return checkInputPath(wfName, st, withKey, inner, parts[1:], inputDoc, consumer, wholeField)
	case "steps":
		if len(parts) < 3 {
			return nil
		}
		return checkStepsOutputPath(g, wfName, st, withKey, inner, parts, byID, consumer, wholeField)
	default:
		return nil
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
		return checkConsumerType(wfName, st, withKey, inner, schema.LookupResult{}, consumer, wholeField)
	}
	got := inputDoc.Lookup(tail)
	if got.Missing {
		return []error{st.Pos.Errorf(
			"workflow %s step %q: ${%s} is not declared in Workflow input schema",
			wfName, strings.TrimSpace(st.ID), inner,
		)}
	}
	return checkConsumerType(wfName, st, withKey, inner, got, consumer, wholeField)
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
) []error {
	prodID := parts[1]
	slot := parts[2]
	if slot != "output" {
		return nil
	}
	prod, ok := byID[prodID]
	if !ok {
		return nil
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
			)}
		}
	}
	return checkConsumerType(wfName, st, withKey, inner, prodLookup, consumer, wholeField)
}

func checkConsumerType(
	wfName string,
	st WorkflowStep,
	withKey, inner string,
	prod schema.LookupResult,
	consumer *schema.Document,
	wholeField bool,
) []error {
	if consumer == nil || strings.TrimSpace(withKey) == "" {
		return nil
	}
	// A single positional agent argument is the whole input document, not a
	// named field. Lowering keys it arg0 because it has no symbol table; the
	// checker already type-checks that shape as the agent's input type (#550).
	lookupPath := []string{withKey}
	inputLabel := withKey
	if isAgentPositionalWholeDocument(st, withKey) {
		lookupPath = nil
		inputLabel = "input"
	}
	cons := consumer.Lookup(lookupPath)
	if cons.Missing {
		return []error{st.Pos.Errorf(
			"workflow %s step %q: with %q is not declared in %s input schema",
			wfName, strings.TrimSpace(st.ID), withKey, consumerSchemaName(st),
		)}
	}
	if !cons.Known {
		return nil
	}
	var prodTypes schema.TypeSet
	srcType := "string"
	if wholeField {
		if !prod.Known {
			return nil
		}
		prodTypes = prod.Types
		srcType = prodTypes.String()
	} else {
		prodTypes = schema.TypeSet{schema.TypeString: {}}
	}
	if schema.Compatible(prodTypes, cons.Types) {
		return nil
	}
	return []error{st.Pos.Errorf(
		"workflow %s step %q: ${%s} (%s) does not match %s input %q (%s)",
		wfName, strings.TrimSpace(st.ID), inner, srcType, consumerSchemaName(st), inputLabel, cons.Types,
	)}
}

// isAgentPositionalWholeDocument reports whether withKey is the lowering
// placeholder for a single positional agent argument. An agent's input is one
// type, not named fields, so arg0 is that document (#550).
func isAgentPositionalWholeDocument(st WorkflowStep, withKey string) bool {
	return strings.TrimSpace(st.Agent) != "" && withKey == "arg0"
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
