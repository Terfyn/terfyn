package spec

import "gopkg.in/yaml.v3"

// MarshalYAML preserves the DAG-mode signal across YAML interchange (issue #207, ADR 003). Needs is
// yaml:"omitempty" and NeedsDeclared is not a YAML field, so a step whose only graph-mode signal is
// an empty declared `needs:` (a parallel root, or an .agent `parallel { }` root the lowerer sets
// NeedsDeclared on with empty Needs) would export without a needs key and reload as implicit
// sequential — silently switching concurrent roots to a chain. When needs is declared but empty,
// emit an explicit `needs: []` so terfyn export → load round-trips to the same graph mode
// ([WorkflowUsesExplicitNeeds]). A non-empty or undeclared needs marshals exactly as the default
// encoder would (this only ever adds the empty sequence). Mirrors [ToolSpec.MarshalYAML].
//
// It also shows the agent call shape (#550). WholeDocument is not a YAML field, and without a
// marker a whole-document step (`Reviewer(value)`) and a named-arg0 step (`Reviewer(arg0: value)`)
// would export byte-identically as `with: {arg0: …}` although the model receives different
// inputs. A whole-document step emits `wholeDocument: true`. That key is deliberately NOT
// accepted by the strict YAML decoder (YAML is not a source under ADR 007, and the bit is set only
// by `.agent` lowering): re-reading an exported stream that contains it fails with an unknown-field
// error instead of silently reloading the step as a named call with a field called arg0.
func (s WorkflowStep) MarshalYAML() (any, error) {
	// alias drops the MarshalYAML method so node.Encode does not recurse.
	type alias WorkflowStep
	var node yaml.Node
	if err := node.Encode(alias(s)); err != nil {
		return nil, err
	}
	if s.NeedsDeclared && len(s.Needs) == 0 {
		injectEmptySequence(&node, "needs")
	}
	if s.WholeDocument {
		injectTrue(&node, "wholeDocument")
	}
	return &node, nil
}

// injectTrue appends `key: true` to mapping node unless key is already present.
func injectTrue(node *yaml.Node, key string) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return
		}
	}
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"},
	)
}
