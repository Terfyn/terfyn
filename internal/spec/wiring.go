package spec

import (
	"regexp"
	"strconv"
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
	// The call-shape invariant holds for every step, synthetic or not: it is what the
	// step would execute under.
	if errs := checkWholeDocumentShape(wfName, st); errs != nil {
		return errs
	}
	// A synthetic (flattened control-flow) step is an effect-analysis
	// over-approximation, not an executable node: its data-dependency needs are not
	// threaded and it is never executed (#305, ADR 002 §5), so its ${steps.*} refs are
	// not held to the executable-graph wiring rules. The checker's type system
	// type-checks its arguments instead.
	if st.Synthetic {
		return nil
	}
	if len(st.With) == 0 {
		return nil
	}
	consumer := consumerInputDoc(g, st)
	var errs []error
	for key, val := range st.With {
		root := wiringSite{key: key, whole: isAgentPositionalWholeDocument(st, key)}
		walkWiringValue(val, root, func(site wiringSite, s string) {
			errs = append(errs, checkWiringString(g, wfName, st, site, s, byID, inputDoc, consumer)...)
		})
	}
	return errs
}

// wiringSite is where one string leaf sits inside a step's with: map, i.e. which
// part of the consumer's input document it supplies.
type wiringSite struct {
	// key is the with: key the leaf is under.
	key string
	// whole is true when key is the placeholder of a whole-document agent call
	// (#550): with[key] is then the agent's whole input document, so the key names
	// no input field and the consumer path starts at the document root.
	whole bool
	// nested is the leaf's path inside with[key]: object keys, and the decimal index
	// of an array element (the form [schema.Document.Lookup] resolves through items).
	nested []string
	// elems are the positions in nested that are array-element segments.
	elems []int
}

// child is the site one level deeper, under object key or array index seg.
func (w wiringSite) child(seg string, elem bool) wiringSite {
	c := w
	c.nested = append(w.nested[:len(w.nested):len(w.nested)], seg)
	if elem {
		c.elems = append(w.elems[:len(w.elems):len(w.elems)], len(w.nested))
	}
	return c
}

// consumerPath is the consumer input-schema path the leaf supplies: the nested path
// under the document root for a whole-document call, else under the with: key.
func (w wiringSite) consumerPath() []string {
	if w.whole {
		return append([]string(nil), w.nested...)
	}
	return append([]string{w.key}, w.nested...)
}

// label names the supplied input location in diagnostics ("input" for the whole
// document itself).
func (w wiringSite) label() string {
	p := w.consumerPath()
	if len(p) == 0 {
		return "input"
	}
	return strings.Join(p, ".")
}

// walkWiringValue calls fn for every string leaf of v with that leaf's site, keeping
// the path inside v so each leaf is checked against the input location it actually
// fills rather than against the whole with: entry.
func walkWiringValue(v any, site wiringSite, fn func(wiringSite, string)) {
	switch t := v.(type) {
	case string:
		fn(site, t)
	case []any:
		for i, e := range t {
			walkWiringValue(e, site.child(strconv.Itoa(i), true), fn)
		}
	case map[string]any:
		for k, e := range t {
			walkWiringValue(e, site.child(k, false), fn)
		}
	}
}

// lookupConsumer resolves the consumer input type at site. An array element is typed
// by the array's items; an array schema that declares no items accepts any element,
// which [schema.Document.Lookup] reports as Missing, so such an element is unknown
// (gradual) rather than undeclared. That exemption applies only when the element's
// parent may be an array (its types are unconstrained or include array): an element
// under a parent that cannot be an array (a closed object, a scalar) is undeclared.
func lookupConsumer(consumer *schema.Document, site wiringSite) schema.LookupResult {
	path := site.consumerPath()
	res := consumer.Lookup(path)
	if !res.Missing {
		return res
	}
	off := len(path) - len(site.nested)
	for _, e := range site.elems {
		i := off + e
		parent := consumer.Lookup(path[:i])
		if parent.Missing {
			break
		}
		if consumer.Lookup(path[:i+1]).Missing {
			if len(parent.Types) == 0 || parent.Types.Has(schema.TypeArray) {
				return schema.LookupResult{}
			}
			break
		}
	}
	return res
}

func checkWiringString(
	g *ProjectGraph,
	wfName string,
	st WorkflowStep,
	site wiringSite,
	s string,
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
		errs = append(errs, checkInterpPath(g, wfName, st, site, inner, byID, inputDoc, consumer, whole && len(tokens) == 1)...)
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
	site wiringSite,
	inner string,
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
		return checkInputPath(wfName, st, site, inner, parts[1:], inputDoc, consumer, wholeField)
	case "steps":
		if len(parts) < 3 {
			return nil
		}
		return checkStepsOutputPath(g, wfName, st, site, inner, parts, byID, consumer, wholeField)
	default:
		return nil
	}
}

func checkInputPath(
	wfName string,
	st WorkflowStep,
	site wiringSite,
	inner string,
	tail []string,
	inputDoc *schema.Document,
	consumer *schema.Document,
	wholeField bool,
) []error {
	if inputDoc == nil {
		return checkConsumerType(wfName, st, site, inner, schema.LookupResult{}, consumer, wholeField)
	}
	got := inputDoc.Lookup(tail)
	if got.Missing {
		return []error{st.Pos.Errorf(
			"workflow %s step %q: ${%s} is not declared in Workflow input schema",
			wfName, strings.TrimSpace(st.ID), inner,
		)}
	}
	return checkConsumerType(wfName, st, site, inner, got, consumer, wholeField)
}

func checkStepsOutputPath(
	g *ProjectGraph,
	wfName string,
	st WorkflowStep,
	site wiringSite,
	inner string,
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
	return checkConsumerType(wfName, st, site, inner, prodLookup, consumer, wholeField)
}

// checkConsumerType checks one interpolation against the consumer input type at the
// location its string fills (site). For a whole-document agent call that location
// is relative to the input document root (#550), so `Reviewer(v)` checks v against
// the whole input and `Reviewer({repo: v})` checks v against the repo field.
func checkConsumerType(
	wfName string,
	st WorkflowStep,
	site wiringSite,
	inner string,
	prod schema.LookupResult,
	consumer *schema.Document,
	wholeField bool,
) []error {
	if consumer == nil || (!site.whole && strings.TrimSpace(site.key) == "") {
		return nil
	}
	cons := lookupConsumer(consumer, site)
	if cons.Missing {
		what := "with"
		if site.whole {
			what = "input field"
		}
		return []error{st.Pos.Errorf(
			"workflow %s step %q: %s %q is not declared in %s input schema",
			wfName, strings.TrimSpace(st.ID), what, site.label(), consumerSchemaName(st),
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
		wfName, strings.TrimSpace(st.ID), inner, srcType, consumerSchemaName(st), site.label(), cons.Types,
	)}
}

// isAgentPositionalWholeDocument reports whether withKey is the single
// positional argument of an agent step, i.e. the agent's whole input document
// (#550). It consumes the explicit [WorkflowStep.WholeDocument] call-shape bit
// that lowering set from the source call; it never infers the shape from the key
// name, so a named call whose field is literally arg0 stays a field lookup, and a
// multi-argument positional call (WholeDocument false) is never treated as one.
func isAgentPositionalWholeDocument(st WorkflowStep, withKey string) bool {
	return st.WholeDocument && strings.TrimSpace(st.Agent) != "" && withKey == WholeDocumentArgKey
}

// checkWholeDocumentShape enforces the [WorkflowStep.WholeDocument] representation
// invariant on every step, synthetic or not, so a snapshot that carries the bit on
// a step that cannot honor it fails loudly instead of executing under a different
// ABI than it validated under: it must be an agent step whose with: is exactly
// the single placeholder entry.
func checkWholeDocumentShape(wfName string, st WorkflowStep) []error {
	if !st.WholeDocument {
		return nil
	}
	if strings.TrimSpace(st.Agent) == "" {
		return []error{st.Pos.Errorf(
			"workflow %s step %q: wholeDocument is only valid on an agent step", wfName, strings.TrimSpace(st.ID))}
	}
	if _, ok := st.With[WholeDocumentArgKey]; !ok || len(st.With) != 1 {
		return []error{st.Pos.Errorf(
			"workflow %s step %q: wholeDocument requires with: to be exactly one positional argument", wfName, strings.TrimSpace(st.ID))}
	}
	return nil
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
