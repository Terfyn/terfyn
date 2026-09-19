package execir

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// ProgramFormatV1 versions the serialized-program payload. An unknown version
// must fail loudly rather than be reinterpreted (SOUNDNESS.md S8): the pinned
// program is execution authority, so a decoder that guessed at an incompatible
// encoding would run something other than what was pinned.
const ProgramFormatV1 = "agentic.dev/execir/v1"

// MarshalPrograms serializes a name→Program map into a canonical, format-versioned
// JSON payload for the deployment snapshot's execution_ir artifact (issue #260).
// It round-trips through [UnmarshalPrograms] to a program with an identical
// [Program.Digest]; positions are omitted (diagnostic-only, already excluded from
// the digest).
func MarshalPrograms(programs map[string]*Program) ([]byte, error) {
	w := programsWire{FormatVersion: ProgramFormatV1, Programs: make(map[string]programWire, len(programs))}
	for name, p := range programs {
		if p == nil {
			continue
		}
		body, err := wireNodes(p.Body)
		if err != nil {
			return nil, fmt.Errorf("execir: encode program %q: %w", name, err)
		}
		w.Programs[name] = programWire{
			Workflow: p.Workflow,
			Params:   p.Params,
			Body:     body,
		}
	}
	return json.Marshal(w)
}

// UnmarshalPrograms decodes a payload written by [MarshalPrograms], failing on an
// unknown format version.
func UnmarshalPrograms(payload []byte) (map[string]*Program, error) {
	var w programsWire
	if err := json.Unmarshal(payload, &w); err != nil {
		return nil, fmt.Errorf("execir: decode programs: %w", err)
	}
	if w.FormatVersion != ProgramFormatV1 {
		return nil, fmt.Errorf("execir: unsupported program format %q (want %q)", w.FormatVersion, ProgramFormatV1)
	}
	out := make(map[string]*Program, len(w.Programs))
	for name, pw := range w.Programs {
		body, err := decodeNodes(pw.Body)
		if err != nil {
			return nil, fmt.Errorf("execir: program %q: %w", name, err)
		}
		out[name] = &Program{Workflow: pw.Workflow, Params: pw.Params, Body: body}
	}
	return out, nil
}

// --- wire types -------------------------------------------------------------

type programsWire struct {
	FormatVersion string                 `json:"formatVersion"`
	Programs      map[string]programWire `json:"programs"`
}

type programWire struct {
	Workflow string     `json:"workflow"`
	Params   []string   `json:"params,omitempty"`
	Body     []nodeWire `json:"body,omitempty"`
}

// nodeWire is a flat tagged union: Kind selects which fields are meaningful.
type nodeWire struct {
	Kind       string             `json:"kind"`
	Bind       string             `json:"bind,omitempty"`
	Uses       string             `json:"uses,omitempty"`
	Agent      string             `json:"agent,omitempty"`
	Workflow   string             `json:"workflow,omitempty"`
	Args       map[string]valWire `json:"args,omitempty"`
	Value      *valWire           `json:"value,omitempty"`
	Cond       *exprWire          `json:"cond,omitempty"`
	Then       []nodeWire         `json:"then,omitempty"`
	Else       []nodeWire         `json:"else,omitempty"`
	Branches   []forkBranchWire   `json:"branches,omitempty"`
	Var        string             `json:"var,omitempty"`
	Parallel   bool               `json:"parallel,omitempty"`
	Collection *valWire           `json:"collection,omitempty"`
	Limit      int                `json:"limit,omitempty"`
	Body       []nodeWire         `json:"body,omitempty"`
	Nodes      []graphNodeWire    `json:"nodes,omitempty"`
	Desc       string             `json:"desc,omitempty"`
	RedactKeys []string           `json:"redactKeys,omitempty"`
}

type forkBranchWire struct {
	Bind  string     `json:"bind,omitempty"`
	Nodes []nodeWire `json:"nodes,omitempty"`
}

type graphNodeWire struct {
	ID    string   `json:"id"`
	Needs []string `json:"needs,omitempty"`
	Run   nodeWire `json:"run"`
}

type valWire struct {
	Kind   string      `json:"kind"`
	Path   []string    `json:"path,omitempty"`
	LitT   string      `json:"litType,omitempty"` // s | i | f | b | nil
	LitV   any         `json:"v,omitempty"`
	Fields []fieldWire `json:"fields,omitempty"`
	Elems  []valWire   `json:"elems,omitempty"`
	Parts  []valWire   `json:"parts,omitempty"`
}

type fieldWire struct {
	Key string  `json:"key"`
	Val valWire `json:"val"`
}

type exprWire struct {
	Kind string    `json:"kind"` // leaf | not | binop
	V    *valWire  `json:"v,omitempty"`
	X    *exprWire `json:"x,omitempty"`
	Y    *exprWire `json:"y,omitempty"`
	Op   string    `json:"op,omitempty"`
}

// --- encode -----------------------------------------------------------------

func wireNodes(nodes []Node) ([]nodeWire, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	out := make([]nodeWire, len(nodes))
	for i, n := range nodes {
		w, err := wireNode(n)
		if err != nil {
			return nil, err
		}
		out[i] = w
	}
	return out, nil
}

func wireNode(n Node) (nodeWire, error) {
	switch v := n.(type) {
	case *InvokeTool:
		args, err := wireArgs(v.Args)
		if err != nil {
			return nodeWire{}, err
		}
		return nodeWire{Kind: "invokeTool", Bind: v.Bind, Uses: v.Uses, Args: args}, nil
	case *InvokeAgent:
		args, err := wireArgs(v.Args)
		if err != nil {
			return nodeWire{}, err
		}
		return nodeWire{Kind: "invokeAgent", Bind: v.Bind, Agent: v.Agent, Args: args}, nil
	case *InvokeWorkflow:
		args, err := wireArgs(v.Args)
		if err != nil {
			return nodeWire{}, err
		}
		return nodeWire{Kind: "invokeWorkflow", Bind: v.Bind, Workflow: v.Workflow, Args: args}, nil
	case *Let:
		vw, err := wireVal(v.Value)
		if err != nil {
			return nodeWire{}, err
		}
		return nodeWire{Kind: "let", Bind: v.Bind, Value: &vw}, nil
	case *Branch:
		ew, err := wireExpr(v.Cond)
		if err != nil {
			return nodeWire{}, err
		}
		then, err := wireNodes(v.Then)
		if err != nil {
			return nodeWire{}, err
		}
		els, err := wireNodes(v.Else)
		if err != nil {
			return nodeWire{}, err
		}
		return nodeWire{Kind: "branch", Cond: &ew, Then: then, Else: els}, nil
	case *Fork:
		brs := make([]forkBranchWire, len(v.Branches))
		for i, b := range v.Branches {
			nodes, err := wireNodes(b.Nodes)
			if err != nil {
				return nodeWire{}, err
			}
			brs[i] = forkBranchWire{Bind: b.Bind, Nodes: nodes}
		}
		return nodeWire{Kind: "fork", Branches: brs}, nil
	case *Loop:
		cw, err := wireVal(v.Collection)
		if err != nil {
			return nodeWire{}, err
		}
		body, err := wireNodes(v.Body)
		if err != nil {
			return nodeWire{}, err
		}
		return nodeWire{Kind: "loop", Var: v.Var, Parallel: v.Parallel, Collection: &cw, Body: body}, nil
	case *While:
		ew, err := wireExpr(v.Cond)
		if err != nil {
			return nodeWire{}, err
		}
		body, err := wireNodes(v.Body)
		if err != nil {
			return nodeWire{}, err
		}
		return nodeWire{Kind: "while", Cond: &ew, Limit: v.Limit, Body: body}, nil
	case *Retry:
		ew, err := wireExpr(v.Cond)
		if err != nil {
			return nodeWire{}, err
		}
		body, err := wireNodes(v.Body)
		if err != nil {
			return nodeWire{}, err
		}
		return nodeWire{Kind: "retry", Cond: &ew, Limit: v.Limit, Body: body}, nil
	case *Return:
		vw, err := wireVal(v.Value)
		if err != nil {
			return nodeWire{}, err
		}
		return nodeWire{Kind: "return", Value: &vw}, nil
	case *Graph:
		gns := make([]graphNodeWire, len(v.Nodes))
		for i, gn := range v.Nodes {
			run, err := wireNode(gn.Run)
			if err != nil {
				return nodeWire{}, err
			}
			gns[i] = graphNodeWire{ID: gn.ID, Needs: gn.Needs, Run: run}
		}
		return nodeWire{Kind: "graph", Nodes: gns}, nil
	case *Approval:
		args, err := wireArgs(v.Args)
		if err != nil {
			return nodeWire{}, err
		}
		return nodeWire{Kind: "approval", Bind: v.Bind, Desc: v.Description, RedactKeys: v.RedactKeys, Args: args}, nil
	default:
		// S8: a program containing a node we cannot faithfully encode must fail rather
		// than be pinned lossy (a "{kind:unknown}" that the digest would not match).
		return nodeWire{}, fmt.Errorf("execir: cannot encode unknown node type %T (S8 fail-closed)", n)
	}
}

func wireArgs(args map[string]Value) (map[string]valWire, error) {
	if len(args) == 0 {
		return nil, nil
	}
	out := make(map[string]valWire, len(args))
	for k, v := range args {
		w, err := wireVal(v)
		if err != nil {
			return nil, err
		}
		out[k] = w
	}
	return out, nil
}

func wireVal(v Value) (valWire, error) {
	switch x := v.(type) {
	case Ref:
		return valWire{Kind: "ref", Path: x.Path}, nil
	case Lit:
		return wireLit(x.V)
	case Object:
		fs := make([]fieldWire, len(x.Fields))
		for i, f := range x.Fields {
			w, err := wireVal(f.Val)
			if err != nil {
				return valWire{}, err
			}
			fs[i] = fieldWire{Key: f.Key, Val: w}
		}
		return valWire{Kind: "object", Fields: fs}, nil
	case List:
		es := make([]valWire, len(x.Elems))
		for i, e := range x.Elems {
			w, err := wireVal(e)
			if err != nil {
				return valWire{}, err
			}
			es[i] = w
		}
		return valWire{Kind: "list", Elems: es}, nil
	case Template:
		ps := make([]valWire, len(x.Parts))
		for i, p := range x.Parts {
			w, err := wireVal(p)
			if err != nil {
				return valWire{}, err
			}
			ps[i] = w
		}
		return valWire{Kind: "template", Parts: ps}, nil
	case nil:
		return valWire{Kind: "lit", LitT: "nil"}, nil
	default:
		return valWire{}, fmt.Errorf("execir: cannot encode unknown value type %T (S8 fail-closed)", v)
	}
}

func wireLit(v any) (valWire, error) {
	switch x := v.(type) {
	case string:
		return valWire{Kind: "lit", LitT: "s", LitV: x}, nil
	case int64:
		// Encode as a decimal STRING, never a JSON number: json.Unmarshal decodes
		// every JSON number into float64 when the target is `any`, rounding an
		// int64 past 2^53 to the nearest double. That would silently hydrate a
		// different program than was pinned (the content digest guards the bytes,
		// not the decoded program). A string is lossless across the full int64.
		return valWire{Kind: "lit", LitT: "i", LitV: strconv.FormatInt(x, 10)}, nil
	case float64:
		return valWire{Kind: "lit", LitT: "f", LitV: x}, nil
	case bool:
		return valWire{Kind: "lit", LitT: "b", LitV: x}, nil
	case nil:
		return valWire{Kind: "lit", LitT: "nil"}, nil
	default:
		// A non-canonical scalar must not be silently coerced (that would change the
		// pinned program relative to its digest); fail closed (S8).
		return valWire{}, fmt.Errorf("execir: cannot encode non-canonical literal of type %T (S8 fail-closed)", v)
	}
}

func wireExpr(e Expr) (exprWire, error) {
	switch x := e.(type) {
	case Leaf:
		vw, err := wireVal(x.V)
		if err != nil {
			return exprWire{}, err
		}
		return exprWire{Kind: "leaf", V: &vw}, nil
	case Not:
		xw, err := wireExpr(x.X)
		if err != nil {
			return exprWire{}, err
		}
		return exprWire{Kind: "not", X: &xw}, nil
	case BinOp:
		xw, err := wireExpr(x.X)
		if err != nil {
			return exprWire{}, err
		}
		yw, err := wireExpr(x.Y)
		if err != nil {
			return exprWire{}, err
		}
		return exprWire{Kind: "binop", Op: x.Op, X: &xw, Y: &yw}, nil
	default:
		return exprWire{}, fmt.Errorf("execir: cannot encode unknown expr type %T (S8 fail-closed)", e)
	}
}

// --- decode -----------------------------------------------------------------

func decodeNodes(nodes []nodeWire) ([]Node, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	out := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		dn, err := decodeNode(n)
		if err != nil {
			return nil, err
		}
		out = append(out, dn)
	}
	return out, nil
}

func decodeNode(n nodeWire) (Node, error) {
	switch n.Kind {
	case "invokeTool":
		args, err := decodeArgs(n.Args)
		if err != nil {
			return nil, err
		}
		return &InvokeTool{Bind: n.Bind, Uses: n.Uses, Args: args}, nil
	case "invokeAgent":
		args, err := decodeArgs(n.Args)
		if err != nil {
			return nil, err
		}
		return &InvokeAgent{Bind: n.Bind, Agent: n.Agent, Args: args}, nil
	case "invokeWorkflow":
		args, err := decodeArgs(n.Args)
		if err != nil {
			return nil, err
		}
		return &InvokeWorkflow{Bind: n.Bind, Workflow: n.Workflow, Args: args}, nil
	case "let":
		val, err := decodeValPtr(n.Value)
		if err != nil {
			return nil, err
		}
		return &Let{Bind: n.Bind, Value: val}, nil
	case "branch":
		cond, err := decodeExprPtr(n.Cond)
		if err != nil {
			return nil, err
		}
		then, err := decodeNodes(n.Then)
		if err != nil {
			return nil, err
		}
		els, err := decodeNodes(n.Else)
		if err != nil {
			return nil, err
		}
		return &Branch{Cond: cond, Then: then, Else: els}, nil
	case "fork":
		brs := make([]ForkBranch, len(n.Branches))
		for i, b := range n.Branches {
			nodes, err := decodeNodes(b.Nodes)
			if err != nil {
				return nil, err
			}
			brs[i] = ForkBranch{Bind: b.Bind, Nodes: nodes}
		}
		return &Fork{Branches: brs}, nil
	case "loop":
		coll, err := decodeValPtr(n.Collection)
		if err != nil {
			return nil, err
		}
		body, err := decodeNodes(n.Body)
		if err != nil {
			return nil, err
		}
		return &Loop{Var: n.Var, Parallel: n.Parallel, Collection: coll, Body: body}, nil
	case "while":
		cond, err := decodeExprPtr(n.Cond)
		if err != nil {
			return nil, err
		}
		body, err := decodeNodes(n.Body)
		if err != nil {
			return nil, err
		}
		return &While{Cond: cond, Limit: n.Limit, Body: body}, nil
	case "retry":
		cond, err := decodeExprPtr(n.Cond)
		if err != nil {
			return nil, err
		}
		body, err := decodeNodes(n.Body)
		if err != nil {
			return nil, err
		}
		return &Retry{Cond: cond, Limit: n.Limit, Body: body}, nil
	case "return":
		val, err := decodeValPtr(n.Value)
		if err != nil {
			return nil, err
		}
		return &Return{Value: val}, nil
	case "graph":
		gns := make([]GraphNode, len(n.Nodes))
		for i, gn := range n.Nodes {
			run, err := decodeNode(gn.Run)
			if err != nil {
				return nil, err
			}
			gns[i] = GraphNode{ID: gn.ID, Needs: gn.Needs, Run: run}
		}
		return &Graph{Nodes: gns}, nil
	case "approval":
		args, err := decodeArgs(n.Args)
		if err != nil {
			return nil, err
		}
		return &Approval{Bind: n.Bind, Description: n.Desc, RedactKeys: n.RedactKeys, Args: args}, nil
	default:
		// S8: an unknown node kind must fail closed, not be dropped — a resumed program
		// missing a step would execute something other than what was pinned.
		return nil, fmt.Errorf("execir: unknown node kind %q (S8 fail-closed)", n.Kind)
	}
}

func decodeArgs(args map[string]valWire) (map[string]Value, error) {
	if len(args) == 0 {
		return nil, nil
	}
	out := make(map[string]Value, len(args))
	for k, v := range args {
		dv, err := decodeVal(v)
		if err != nil {
			return nil, err
		}
		out[k] = dv
	}
	return out, nil
}

func decodeValPtr(v *valWire) (Value, error) {
	if v == nil {
		return nil, nil
	}
	return decodeVal(*v)
}

func decodeVal(v valWire) (Value, error) {
	switch v.Kind {
	case "ref":
		return Ref{Path: v.Path}, nil
	case "lit":
		lit, err := decodeLit(v)
		if err != nil {
			return nil, err
		}
		return Lit{V: lit}, nil
	case "object":
		fs := make([]Field, len(v.Fields))
		for i, f := range v.Fields {
			fv, err := decodeVal(f.Val)
			if err != nil {
				return nil, err
			}
			fs[i] = Field{Key: f.Key, Val: fv}
		}
		return Object{Fields: fs}, nil
	case "list":
		es := make([]Value, len(v.Elems))
		for i, e := range v.Elems {
			ev, err := decodeVal(e)
			if err != nil {
				return nil, err
			}
			es[i] = ev
		}
		return List{Elems: es}, nil
	case "template":
		ps := make([]Value, len(v.Parts))
		for i, p := range v.Parts {
			pv, err := decodeVal(p)
			if err != nil {
				return nil, err
			}
			ps[i] = pv
		}
		return Template{Parts: ps}, nil
	default:
		return nil, fmt.Errorf("execir: unknown value kind %q (S8 fail-closed)", v.Kind)
	}
}

func decodeLit(v valWire) (any, error) {
	switch v.LitT {
	case "s":
		if s, ok := v.LitV.(string); ok {
			return s, nil
		}
		return nil, fmt.Errorf("execir: string literal has non-string value %T", v.LitV)
	case "i":
		// Decimal string (see wireLit) — lossless across the full int64 range.
		if s, ok := v.LitV.(string); ok {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("execir: int literal %q: %w", s, err)
			}
			return n, nil
		}
		return nil, fmt.Errorf("execir: int literal has non-string value %T", v.LitV)
	case "f":
		if f, ok := v.LitV.(float64); ok {
			return f, nil
		}
		return nil, fmt.Errorf("execir: float literal has non-number value %T", v.LitV)
	case "b":
		if b, ok := v.LitV.(bool); ok {
			return b, nil
		}
		return nil, fmt.Errorf("execir: bool literal has non-bool value %T", v.LitV)
	case "nil":
		return nil, nil
	default:
		return nil, fmt.Errorf("execir: unknown literal type %q (S8 fail-closed)", v.LitT)
	}
}

func decodeExprPtr(e *exprWire) (Expr, error) {
	if e == nil {
		return nil, nil
	}
	return decodeExpr(*e)
}

func decodeExpr(e exprWire) (Expr, error) {
	switch e.Kind {
	case "leaf":
		val, err := decodeValPtr(e.V)
		if err != nil {
			return nil, err
		}
		return Leaf{V: val}, nil
	case "not":
		x, err := decodeExprPtr(e.X)
		if err != nil {
			return nil, err
		}
		return Not{X: x}, nil
	case "binop":
		x, err := decodeExprPtr(e.X)
		if err != nil {
			return nil, err
		}
		y, err := decodeExprPtr(e.Y)
		if err != nil {
			return nil, err
		}
		return BinOp{Op: e.Op, X: x, Y: y}, nil
	default:
		// S8: an unknown expression kind must fail closed — decoding it as an
		// always-false leaf would silently take a branch's else arm.
		return nil, fmt.Errorf("execir: unknown expr kind %q (S8 fail-closed)", e.Kind)
	}
}
