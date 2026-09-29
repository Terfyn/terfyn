package lower

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/lang"
	"github.com/Terfyn/terfyn/internal/spec"
)

// returnShapeFixture is one representative program, lowered by the real YAML or
// `.agent` lowering, with the digest and wire-form hash main compiled it to
// (pinned at 8741333, the released baseline; identical at 2114079). The output
// shape is not part of a program's identity, so every one of them must keep
// those bytes.
type returnShapeFixture struct {
	name       string
	agent      string         // `.agent` source (first decl is the workflow), or ""
	yamlOut    map[string]any // YAML output.value when agent == ""
	yamlNoOut  bool           // YAML workflow with no output block
	digest     string
	wireSHA256 string
	shape      ReturnShape
}

func (fx returnShapeFixture) lower(t *testing.T) (*execir.Program, *spec.WorkflowResource) {
	t.Helper()
	if fx.agent != "" {
		return lowerAgentBoth(t, fx.agent)
	}
	wf := &spec.WorkflowResource{Metadata: spec.Metadata{Name: "w"}}
	wf.Spec.Steps = []spec.WorkflowStep{{ID: "s", Uses: "helper.echo", With: map[string]any{"msg": "${input.a}"}}}
	if !fx.yamlNoOut {
		wf.Spec.Output = &spec.WorkflowOutput{Value: fx.yamlOut}
	}
	prog, diags := LowerWorkflowResource(wf)
	if diags.HasErrors() {
		t.Fatalf("%s: %v", fx.name, diags)
	}
	return prog, wf
}

// lowerAgentBoth lowers the first workflow in src into both projections: the
// execution program (LowerExec) and the flattened resource (LowerFile) the checker
// and engine pass to [WorkflowReturnShape] alongside it.
func lowerAgentBoth(t *testing.T, src string) (*execir.Program, *spec.WorkflowResource) {
	t.Helper()
	f, diags := lang.Parse("test.agent", src)
	if diags.HasErrors() {
		t.Fatalf("parse errors: %v", diags)
	}
	wd, ok := f.Decls[0].(*lang.WorkflowDecl)
	if !ok {
		t.Fatalf("first decl is not a workflow")
	}
	prog, diags := LowerExec(wd, nil)
	if diags.HasErrors() {
		t.Fatalf("LowerExec: %v", diags)
	}
	res, diags := LowerFile(f, Options{})
	if diags.HasErrors() {
		t.Fatalf("LowerFile: %v", diags)
	}
	for _, wf := range res.Workflows {
		if wf.Metadata.Name == prog.Workflow {
			return prog, wf
		}
	}
	t.Fatalf("LowerFile produced no workflow %q", prog.Workflow)
	return nil, nil
}

var returnShapeFixtures = []returnShapeFixture{
	{name: "agent scalar return", agent: `
workflow W(input: Anything) -> Anything {
    return input.a
}`,
		digest:     "7fc6fdd18429580ee326e1d6201fc5a8c8fc3cd0aa296929eae87951ed4147d8",
		wireSHA256: "1ff053112ed6af20a81626bd771b9738c795eba0bb4f792e53a67b201cae4a44",
		shape:      ReturnValueEnvelope,
	},
	{name: "agent object return", agent: `
workflow W(input: Anything) -> Anything {
    return { r: input.a }
}`,
		digest:     "b6d8517333c5011b525df83c6ae62abb46d4f708dbdfc9271af3a6639c3c3535",
		wireSHA256: "824162cc9c33f5cd9d437d7743043b23fb87fa0ce271ca5066a0599f70db9e50",
		shape:      ReturnDocument,
	},
	{name: "agent multi-key object with value", agent: `
workflow W(input: Anything) -> Anything {
    return { value: input.a, r: input.b }
}`,
		digest:     "1f3e0cbf4c1909fa09c29a39cfcf8f51666fa526b5504ef1fbdb482b9be85354",
		wireSHA256: "74848a41043aa0494ab810dde4c3483d59c2087bdcb3f72a5498b4bac0e4278b",
		shape:      ReturnDocument,
	},
	// A lone `{value: x}` literal is the document (review #578) and keeps main's
	// digest and wire bytes.
	{name: "agent lone value literal (V1)", agent: `
workflow W(input: Anything) -> Anything {
    return { value: input.b }
}`,
		digest:     "5e303776b8d96aa80ba6afc2fa6f7a4bea7c6926eb3120a9895ad0a7abfb60be",
		wireSHA256: "b727acd562811367aac57025a65f98d10c94653ed29e02dc16c168781b86fb13",
		shape:      ReturnDocument,
	},
	{name: "agent lone value literal in if without else (V3)", agent: `
workflow W(input: Anything) -> Anything {
    if input.flag {
        return { value: input.b }
    }
}`,
		digest:     "c54f125f519a0beac23b465e85d89c8ab36c55a786079e81179f6275a2272806",
		wireSHA256: "5929e7c9d63e25629b4a86b703743487c2b2366c1f6872648bd36d8ebafb9521",
		shape:      ReturnDocument,
	},
	{name: "agent lone value string literal", agent: `
workflow W(input: Anything) -> Anything {
    return { value: "lit" }
}`,
		digest:     "7ea304afc83e05c406f6b8cd524870c13f07e6a8178869fc2c7c07a925a6c65c",
		wireSHA256: "a043195a15ee3473bba4736e8363ae8f2e45f6055be5d8c3bb5cb918f3907436",
		shape:      ReturnDocument,
	},
	// `return {value: <object literal>}`: its resource `output.value` is
	// `{value: <map>}`, the shape of the YAML envelope around a map, but the
	// Return is one level deeper than that map, so it does not mirror it: it is
	// the document and keeps main's digest and wire bytes.
	{name: "agent value around an object (PMap)", agent: `
workflow W(input: Anything) -> Anything {
    return { value: { x: input.b } }
}`,
		digest:     "5c4f1c9d3e1e355600d2798a5a7c27e59eeab0fe743fb4f06a296eaa389e2d33",
		wireSHA256: "583548b1d3d4dffc7338cc1bc4baf8b8e97597d0be58a5cfaabc1f9ac357f4a8",
		shape:      ReturnDocument,
	},
	{name: "agent value around a value object (PVV)", agent: `
workflow W(input: Anything) -> Anything {
    return { value: { value: input.b } }
}`,
		digest:     "0736125cd8b55ce8bb6d4e8319755d2f8748393f7b5b8a82c5172dc17c99ee91",
		wireSHA256: "0dcc1fda55ed3f2c9a4cf29e7d60d93e743c6e222fbb2d4f6119dc8ab6835d48",
		shape:      ReturnDocument,
	},
	{name: "agent value around a literal object (FreeMap)", agent: `
workflow W(input: Anything) -> Anything {
    return { value: { x: "y" } }
}`,
		digest:     "3747c525fba6d9842a27de97721fb75b57b0732ec5c7d700e58971b6931466bc",
		wireSHA256: "92d0b0eecd663275fda246f90e7952b08bd9b851517350b87e17ce54c561c605",
		shape:      ReturnDocument,
	},
	{name: "agent two value arms", agent: `
workflow W(input: Anything) -> Anything {
    if input.flag {
        return { value: input.a }
    } else {
        return { value: input.b }
    }
}`,
		digest:     "489b72d737423343c66a45a90bb175f0b109b5c852f946ba230bad9038f3f80c",
		wireSHA256: "57b4caf2d2112140ca70f02feba0b9f644f75187e5b423f68bef6e9354a861b0",
		shape:      ReturnDocument,
	},
	{name: "agent mixed arms", agent: `
workflow W(input: Anything) -> Anything {
    if input.flag {
        return { r: input.a }
    } else {
        return input.b
    }
}`,
		digest:     "a6cdeb2b1bf692449d9d37ab0eb94e2dc9003897e36cd2160837eb9cf855fc11",
		wireSHA256: "75fb957e041fefde2b7089ed2a39b7e68c17d3ebb4eb9e53cb7088487bcbf411",
		shape:      ReturnValueEnvelope,
	},
	{name: "agent no return", agent: `
workflow W(input: Anything) -> Anything {
    x = helper.echo(msg: input.a)
}`,
		digest:     "a33d1930364154fabce8d27790243ed6160b56c4cdde110ea632f49d5987943c",
		wireSHA256: "3b6f50c5389e16829c33b3fd96436eb56f4dbdc0fa3234a5df5ede213706e6b5",
		shape:      ReturnNone,
	},
	{name: "yaml single value", yamlOut: map[string]any{"value": "${input.a}"},
		digest:     "a159e4d237f59d1f7ac17d41fc41d6703be6b5411eca0484e440f83bd65b0f42",
		wireSHA256: "157a40b62eaeeb6c76b522a87eb335162315de524de9715df63f45edba33ec53",
		shape:      ReturnValueEnvelope,
	},
	{name: "yaml value around a map", yamlOut: map[string]any{"value": map[string]any{"a": "${input.a}"}},
		digest:     "bcbb2a6edcecc9f2d80f6ef4ec18ef3a0290e0c48dfa35c76fe2254dd92571ff",
		wireSHA256: "351fc8d030ae8eb847b1ca79e7834517e33f54e7de385c255406b57ecc1ef1a5",
		shape:      ReturnValueEnvelope,
	},
	{name: "yaml single other key", yamlOut: map[string]any{"got": "${input.a}"},
		digest:     "09f3afc733d65a13e28b0ecd0781215754c4be4fe17762b5a8878a9ee00ba516",
		wireSHA256: "e7785b30488fe7ce81240b17be8cd62224919cae5624b285a3237986c904a3f5",
		shape:      ReturnDocument,
	},
	{name: "yaml multi key", yamlOut: map[string]any{"a": "${input.a}", "value": "${input.b}"},
		digest:     "1b21aa9acd4291c9094db6380e25f581037c45593f3e6385f74fec58df6cefc2",
		wireSHA256: "c79871d16e84c2f8cbc8ac3b1f4266bb901016ab81c79cbdc6c853cebe692b3b",
		shape:      ReturnDocument,
	},
	{name: "yaml no output", yamlNoOut: true,
		digest:     "94956f2c60b735908c1946975c8094132f444f0059ee1e1962a3ef465a6d64bc",
		wireSHA256: "f6d5b027faa1a6988aadbe238374cc270e492e7f2cdcc3597898458ac9496ba8",
		shape:      ReturnNone,
	},
}

func wireSHA(t *testing.T, prog *execir.Program) string {
	t.Helper()
	b, err := execir.MarshalPrograms(map[string]*execir.Program{"w": prog})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestReturnShape_programsKeepMainDigestAndWire pins the digest and wire bytes
// of representative YAML and `.agent` programs, including every lone
// `return {value: …}` form, to what main compiled: the output shape is computed
// from the Return nodes and resource, never recorded in the program, so no plan
// goes stale, every program pinned on main round-trips to the same bytes, and
// it is classified as a fresh compile is.
func TestReturnShape_programsKeepMainDigestAndWire(t *testing.T) {
	t.Parallel()
	for _, fx := range returnShapeFixtures {
		prog, wf := fx.lower(t)
		if got := prog.Digest(); got != fx.digest {
			t.Errorf("%s: digest changed: %s, want %s", fx.name, got, fx.digest)
		}
		b, err := execir.MarshalPrograms(map[string]*execir.Program{"w": prog})
		if err != nil {
			t.Fatal(err)
		}
		if got := wireSHA(t, prog); got != fx.wireSHA256 {
			t.Errorf("%s: wire bytes changed: sha256 %s, want %s (%s)", fx.name, got, fx.wireSHA256, b)
		}
		if got := WorkflowReturnShape(prog, wf); got != fx.shape {
			t.Errorf("%s: shape %v, want %v", fx.name, got, fx.shape)
		}
		back, err := execir.UnmarshalPrograms(b)
		if err != nil {
			t.Fatal(err)
		}
		if got := back["w"].Digest(); got != fx.digest {
			t.Errorf("%s: round trip changed the digest: %s", fx.name, got)
		}
		if got := WorkflowReturnShape(back["w"], wf); got != fx.shape {
			t.Errorf("%s: round-tripped shape %v, want %v", fx.name, got, fx.shape)
		}
	}
}
