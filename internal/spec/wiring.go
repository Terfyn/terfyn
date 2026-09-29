package spec

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Terfyn/terfyn/internal/schema"
)

// interpTokenRE matches ${...} placeholders (design doc §13.1). Same shape as engine/interpolation.go.
var interpTokenRE = regexp.MustCompile(`\$\{([^}]*)\}`)

// validateStepWiring checks each agent step's `with` against the consumer agent's declared input
// schema on the graph (issue #193). Every with-key is a flow into the consumer input, and every
// flow goes through schema.CompatibleLookup, the same rule the .agent checker applies. Where a
// with-key lands is decided by the step's explicit call shape, never by the key's name: normally
// at the input field of that name, but for the single positional argument of a whole-document
// agent call ([WorkflowStep.WholeDocument], keyed [WholeDocumentArgKey]) at the input root (#550).
// A string leaf nested inside an object/array value is checked at its path under that location.
//
//   - a ${steps.*.output...} / ${input...} interpolation is typed by the producer's schema at that
//     path (an embedded token renders into a string);
//   - a with-key whose value contains no token at all — a string, number, bool, null, or an
//     object/array built only from those — is an untyped producer (schema.LookupResult{}), exactly
//     as the .agent checker types a literal argument (LitExpr / ObjectExpr are untyped). That is
//     gradual against every typed or untyped consumer, but still subject to the key being declared
//     and rejected by a never (false) consumer. A value that mixes literals and typed tokens is
//     checked through those tokens; a value whose only tokens this pass does not type
//     (${steps.<id>.status}, a bare ${input}, an unknown step, ...) is an untyped producer too,
//     so no with-key is left unchecked;
//   - a step with no `with` at all into a consumer whose whole input is never is rejected, matching
//     the .agent checker's zero-argument error — agent input is not validated at run time, so this
//     static check is what stops a false-input agent from running.
//
// A multi-argument positional .agent call (WholeDocument false, keys arg0, arg1, ...) is an
// undefined ABI the checker warns about; its keys are checked as input fields, as the runtime
// sends them. Absent schemas are gradual.
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
		// root is where with[key] lands in the consumer input: the field key, or — for the
		// single positional argument of a whole-document agent call (#550) — the input root.
		root := wiringSite{key: key, whole: isAgentPositionalWholeDocument(st, key)}
		// covered: some token in the value was typed and checked against the consumer.
		hasToken, covered := false, false
		walkWiringValue(st.With[key], root, func(site wiringSite, s string) {
			tokErrs, tokCovered, tokFound := checkWiringString(g, wfName, st, site, s, byID, inputDoc, consumer)
			errs = append(errs, tokErrs...)
			covered = covered || tokCovered
			hasToken = hasToken || tokFound
		})
		if covered {
			continue
		}
		// No typed flow reached the consumer: a literal, or only tokens this pass does not type
		// (e.g. ${steps.<id>.status}, ${input}, an unknown step). Either way the value is an
		// untyped producer, checked at root: the field key, or the input root for a whole-document
		// agent argument — never as a field named after the lowering placeholder.
		what := "literal value"
		if hasToken {
			what = "untyped value"
		}
		errs = append(errs, checkConsumerType(wfName, st, root, what, schema.LookupResult{}, consumer, true)...)
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
) (errs []error, covered, found bool) {
	tokens, whole := interpTokens(s)
	if len(tokens) == 0 {
		return nil, false, false
	}
	for _, inner := range tokens {
		e, c := checkInterpPath(g, wfName, st, site, inner, byID, inputDoc, consumer, whole && len(tokens) == 1)
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
	site wiringSite,
	inner string,
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
		return checkInputPath(wfName, st, site, inner, parts[1:], inputDoc, consumer, wholeField), true
	case "steps":
		if len(parts) < 3 {
			return nil, false
		}
		return checkStepsOutputPath(g, wfName, st, site, inner, parts, byID, consumer, wholeField)
	default:
		return nil, false
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
		return checkConsumerType(wfName, st, site, "${"+inner+"}", schema.LookupResult{}, consumer, wholeField)
	}
	got := inputDoc.Lookup(tail)
	if got.Missing {
		return []error{st.Pos.Errorf(
			"workflow %s step %q: ${%s} is not declared in Workflow input schema",
			wfName, strings.TrimSpace(st.ID), inner,
		)}
	}
	return checkConsumerType(wfName, st, site, "${"+inner+"}", got, consumer, wholeField)
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
	return checkConsumerType(wfName, st, site, "${"+inner+"}", prodLookup, consumer, wholeField), true
}

// checkConsumerType checks one producer flowing into the consumer input at the location its
// value fills (site). For a whole-document agent call that location is relative to the input
// document root (#550), so `Reviewer(v)` checks v against the whole input and
// `Reviewer({repo: v})` checks v against the repo field. what names the producer in
// diagnostics: "${<path>}" for an interpolation token, "literal value" / "untyped value" for a
// with value this pass does not type.
func checkConsumerType(
	wfName string,
	st WorkflowStep,
	site wiringSite,
	what string,
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
		wfName, strings.TrimSpace(st.ID), what, src, consumerSchemaName(st), site.label(), cons,
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
