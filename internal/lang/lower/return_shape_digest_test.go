package lower

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/lang"
	"github.com/Terfyn/terfyn/internal/spec"
)

// returnShapeFixture is one representative program, lowered by the real YAML or
// `.agent` lowering, with the digest and wire-form hash it had BEFORE
// [execir.Program.DocumentReturn] existed (pinned at 2333a68).
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

// TestDocumentReturn_existingProgramsUnchanged pins the digest and wire bytes of
// representative YAML and ordinary `.agent` programs from before the
// DocumentReturn bit existed: none of them is marked, so none of their plans goes
// stale and every pinned snapshot program round-trips to the same bytes.
func TestDocumentReturn_existingProgramsUnchanged(t *testing.T) {
	t.Parallel()
	for _, fx := range returnShapeFixtures {
		prog, wf := fx.lower(t)
		if prog.DocumentReturn {
			t.Errorf("%s: DocumentReturn set on a program that does not need it", fx.name)
		}
		if got := prog.Digest(); got != fx.digest {
			t.Errorf("%s: digest changed: %s, want %s", fx.name, got, fx.digest)
		}
		if got := wireSHA(t, prog); got != fx.wireSHA256 {
			t.Errorf("%s: wire bytes changed: sha256 %s, want %s", fx.name, got, fx.wireSHA256)
		}
		if got := WorkflowReturnShape(prog, wf); got != fx.shape {
			t.Errorf("%s: shape %v, want %v", fx.name, got, fx.shape)
		}
	}
}

// TestDocumentReturn_loneValueObjectReturn: a `.agent` program whose one Return is
// a `{value: …}` literal (review #578 V1, and V3 where the Return sits in an
// if-without-else) is marked, so its output is the returned object — the same
// document a two-arm `{value: …}` program produces. The mark is identity: the
// digest and wire form differ from the unmarked program pinned at 2333a68, and
// round-trip. That unmarked program (a snapshot pinned before the bit existed)
// keeps the envelope it had.
func TestDocumentReturn_loneValueObjectReturn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, src, oldDigest string
	}{
		{"V1 single return", `
workflow W(input: Anything) -> Anything {
    return { value: input.b }
}`, "5e303776b8d96aa80ba6afc2fa6f7a4bea7c6926eb3120a9895ad0a7abfb60be"},
		{"V3 return in if without else", `
workflow W(input: Anything) -> Anything {
    if input.flag {
        return { value: input.b }
    }
}`, "c54f125f519a0beac23b465e85d89c8ab36c55a786079e81179f6275a2272806"},
	} {
		prog, wf := lowerAgentBoth(t, tc.src)
		if !prog.DocumentReturn {
			t.Fatalf("%s: DocumentReturn not set", tc.name)
		}
		if got := WorkflowReturnShape(prog, wf); got != ReturnDocument {
			t.Errorf("%s: shape %v, want ReturnDocument", tc.name, got)
		}
		d := prog.Digest()
		if d == tc.oldDigest {
			t.Errorf("%s: marked program kept the unmarked digest %s", tc.name, d)
		}
		b, err := execir.MarshalPrograms(map[string]*execir.Program{"w": prog})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `"documentReturn":true`) {
			t.Errorf("%s: wire form lacks the bit: %s", tc.name, b)
		}
		back, err := execir.UnmarshalPrograms(b)
		if err != nil {
			t.Fatal(err)
		}
		if !back["w"].DocumentReturn || back["w"].Digest() != d {
			t.Errorf("%s: round trip lost the bit or changed the digest", tc.name)
		}

		old := *prog
		old.DocumentReturn = false
		if got := old.Digest(); got != tc.oldDigest {
			t.Errorf("%s: unmarked digest %s, want the 2333a68 pin %s", tc.name, got, tc.oldDigest)
		}
		if got := WorkflowReturnShape(&old, wf); got != ReturnValueEnvelope {
			t.Errorf("%s: pinned unmarked program shape %v, want ReturnValueEnvelope", tc.name, got)
		}
	}
}
