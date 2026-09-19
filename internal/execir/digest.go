package execir

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Digest returns a hex SHA-256 over a canonical, position-independent encoding
// of the program's executable shape. ADR 002 §5 requires the execution-IR digest
// to fold into the workflow hash so a lowering change with no resource-level
// change still invalidates a stale plan (internal/plan.WorkflowSpecHashWithExec).
//
// Positions are deliberately excluded: moving a workflow within a file must not
// change what it executes, matching the resource projection's own
// canonicalization. Two programs share a digest iff they invoke the same
// operations with the same arguments under the same control flow and binding
// names.
func (p *Program) Digest() string {
	var b strings.Builder
	if p != nil {
		b.WriteString("wf(")
		b.WriteString(p.Workflow)
		b.WriteString(")params[")
		b.WriteString(strings.Join(p.Params, ","))
		b.WriteString("]")
		encodeNodes(&b, p.Body)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func encodeNodes(b *strings.Builder, nodes []Node) {
	b.WriteByte('{')
	for _, n := range nodes {
		encodeNode(b, n)
		b.WriteByte(';')
	}
	b.WriteByte('}')
}

func encodeNode(b *strings.Builder, n Node) {
	switch v := n.(type) {
	case *InvokeTool:
		b.WriteString("tool ")
		b.WriteString(v.Bind)
		b.WriteByte('=')
		b.WriteString(v.Uses)
		encodeArgs(b, v.Args)
	case *InvokeAgent:
		b.WriteString("agent ")
		b.WriteString(v.Bind)
		b.WriteByte('=')
		b.WriteString(v.Agent)
		encodeArgs(b, v.Args)
	case *InvokeWorkflow:
		b.WriteString("workflow ")
		b.WriteString(v.Bind)
		b.WriteByte('=')
		b.WriteString(v.Workflow)
		encodeArgs(b, v.Args)
	case *Let:
		b.WriteString("let ")
		b.WriteString(v.Bind)
		b.WriteByte('=')
		encodeValue(b, v.Value)
	case *Branch:
		b.WriteString("branch ")
		encodeExpr(b, v.Cond)
		b.WriteString("then")
		encodeNodes(b, v.Then)
		b.WriteString("else")
		encodeNodes(b, v.Else)
	case *Fork:
		b.WriteString("fork")
		for _, br := range v.Branches {
			b.WriteString(br.Bind)
			b.WriteByte(':')
			encodeNodes(b, br.Nodes)
		}
	case *Loop:
		b.WriteString("loop ")
		if v.Parallel {
			b.WriteString("par ")
		}
		b.WriteString(v.Var)
		b.WriteString(" in ")
		encodeValue(b, v.Collection)
		encodeNodes(b, v.Body)
	case *While:
		b.WriteString("while ")
		encodeExpr(b, v.Cond)
		b.WriteString(" limit ")
		b.WriteString(strconv.Itoa(v.Limit))
		encodeNodes(b, v.Body)
	case *Retry:
		b.WriteString("retry until ")
		encodeExpr(b, v.Cond)
		b.WriteString(" limit ")
		b.WriteString(strconv.Itoa(v.Limit))
		encodeNodes(b, v.Body)
	case *Return:
		b.WriteString("return ")
		encodeValue(b, v.Value)
	case *Graph:
		encodeGraph(b, v)
	case *Approval:
		b.WriteString("approval ")
		b.WriteString(v.Bind)
		b.WriteString(" desc:")
		b.WriteString(strconv.Quote(v.Description))
		b.WriteString(" redact:[")
		b.WriteString(strings.Join(sortedCopy(v.RedactKeys), ","))
		b.WriteByte(']')
		encodeArgs(b, v.Args)
	default:
		b.WriteString("?node")
	}
}

// encodeGraph canonicalizes a needs-DAG independently of authored node order:
// nodes are emitted sorted by id and each node's needs are sorted, so reordering
// independent steps (a semantically-equivalent structuring) yields one digest.
func encodeGraph(b *strings.Builder, g *Graph) {
	b.WriteString("graph{")
	nodes := make([]GraphNode, len(g.Nodes))
	copy(nodes, g.Nodes)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	for _, gn := range nodes {
		b.WriteString(gn.ID)
		b.WriteByte('[')
		b.WriteString(strings.Join(sortedCopy(gn.Needs), ","))
		b.WriteString("]=")
		encodeNode(b, gn.Run)
		b.WriteByte(';')
	}
	b.WriteByte('}')
}

// sortedCopy returns a sorted copy of ss, leaving the input untouched (the digest
// must not mutate the program it hashes).
func sortedCopy(ss []string) []string {
	out := make([]string, len(ss))
	copy(out, ss)
	sort.Strings(out)
	return out
}

func encodeArgs(b *strings.Builder, args map[string]Value) {
	b.WriteByte('(')
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		encodeValue(b, args[k])
		b.WriteByte(',')
	}
	b.WriteByte(')')
}

func encodeValue(b *strings.Builder, v Value) {
	switch x := v.(type) {
	case Ref:
		b.WriteString("ref:")
		b.WriteString(strings.Join(x.Path, "."))
	case Lit:
		b.WriteString("lit:")
		b.WriteString(litKey(x.V))
	case Object:
		b.WriteString("obj{")
		fields := make([]Field, len(x.Fields))
		copy(fields, x.Fields)
		sort.Slice(fields, func(i, j int) bool { return fields[i].Key < fields[j].Key })
		for _, f := range fields {
			b.WriteString(f.Key)
			b.WriteByte('=')
			encodeValue(b, f.Val)
			b.WriteByte(',')
		}
		b.WriteByte('}')
	case List:
		b.WriteString("list[")
		for _, e := range x.Elems {
			encodeValue(b, e)
			b.WriteByte(',')
		}
		b.WriteByte(']')
	case Template:
		b.WriteString("tmpl(")
		for _, p := range x.Parts {
			encodeValue(b, p)
			b.WriteByte(';')
		}
		b.WriteByte(')')
	case nil:
		b.WriteString("nil")
	default:
		b.WriteString("?val")
	}
}

func encodeExpr(b *strings.Builder, e Expr) {
	switch x := e.(type) {
	case Leaf:
		encodeValue(b, x.V)
	case Not:
		b.WriteString("!(")
		encodeExpr(b, x.X)
		b.WriteByte(')')
	case BinOp:
		b.WriteByte('(')
		encodeExpr(b, x.X)
		b.WriteString(x.Op)
		encodeExpr(b, x.Y)
		b.WriteByte(')')
	case nil:
		b.WriteString("nilexpr")
	default:
		b.WriteString("?expr")
	}
}

// litKey renders a literal so distinct types never collide (1 the int vs "1"
// the string).
func litKey(v any) string {
	switch x := v.(type) {
	case string:
		return "s" + strconv.Quote(x)
	case int64:
		return "i" + strconv.FormatInt(x, 10)
	case float64:
		return "f" + strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		return "b" + strconv.FormatBool(x)
	default:
		return "x" + fmt.Sprintf("%v", x)
	}
}
