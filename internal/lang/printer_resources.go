package lang

import (
	"fmt"
	"strconv"
	"strings"
)

// Canonical printing for inline tool/policy declarations (ADR 005, issue #333). Fields are
// newline-separated, matching the rest of the .agent surface.

func printTool(p *printer, d *ToolDecl) {
	fmt.Fprintf(p, "tool %s {\n", identName(d.Name))
	if d.Type != nil {
		p.leadingBefore(d.Type.Pos.Line, "    ")
		p.field("    ", "type "+d.Type.Name, d.Type.Pos.Line)
	}
	if m := d.MCP; m != nil {
		p.leadingBefore(m.Pos.Line, "    ")
		p.WriteString("    mcp {\n")
		printStringLitField(p, "        ", "transport", m.Transport)
		printStringLitField(p, "        ", "command", m.Command)
		if len(m.Args) > 0 {
			p.WriteString("        args {")
			for _, a := range m.Args {
				fmt.Fprintf(p, " %s", strconv.Quote(a.Value))
			}
			p.WriteString(" }\n")
		}
		printStringLitField(p, "        ", "url", m.URL)
		printHeadersBlock(p, "        ", m.Headers)
		p.blockTail(m.Pos.Line, "        ")
		p.WriteString("    }\n")
	}
	if h := d.HTTP; h != nil {
		p.leadingBefore(h.Pos.Line, "    ")
		p.WriteString("    http {\n")
		printStringLitField(p, "        ", "baseUrl", h.BaseURL)
		printHeadersBlock(p, "        ", h.Headers)
		p.blockTail(h.Pos.Line, "        ")
		p.WriteString("    }\n")
	}
	if w := d.Workspace; w != nil {
		p.leadingBefore(w.Pos.Line, "    ")
		p.WriteString("    workspace {\n")
		printStringLitField(p, "        ", "root", w.Root)
		printStringLitField(p, "        ", "testCommand", w.TestCommand)
		p.blockTail(w.Pos.Line, "        ")
		p.WriteString("    }\n")
	}
	if r := d.Retry; r != nil {
		p.leadingBefore(r.Pos.Line, "    ")
		p.WriteString("    retry {\n")
		if r.MaxAttempts != nil {
			fmt.Fprintf(p, "        maxAttempts %d\n", *r.MaxAttempts)
		}
		printStringLitField(p, "        ", "backoff", r.Backoff)
		p.blockTail(r.Pos.Line, "        ")
		p.WriteString("    }\n")
	}
	if lim := d.Limits; lim != nil {
		p.leadingBefore(lim.Pos.Line, "    ")
		printLimitsBlockAt(p, "    ", lim)
	}
	if s := d.Safety; s != nil {
		p.leadingBefore(s.Pos.Line, "    ")
		p.WriteString("    safety {\n")
		printBoolField(p, "        ", "trusted", s.Trusted)
		printBoolField(p, "        ", "sideEffects", s.SideEffects)
		printBoolField(p, "        ", "requiresApproval", s.RequiresApproval)
		p.blockTail(s.Pos.Line, "        ")
		p.WriteString("    }\n")
	}
	if d.Operations != nil {
		if len(d.Operations.Ops) == 0 {
			p.leadingBefore(d.Operations.Pos.Line, "    ")
			p.WriteString("    operations {}\n") // an explicit empty block: a closed, deny-all manifest
		} else {
			p.leadingBefore(d.Operations.Pos.Line, "    ")
			p.WriteString("    operations {\n")
			for _, op := range d.Operations.Ops {
				p.leadingBefore(op.Pos.Line, "        ")
				line := identName(op.Name) + " {"
				if op.Schema != nil {
					line += " schema " + strconv.Quote(op.Schema.Value)
				}
				if len(op.Effects) > 0 {
					line += " effects { " + joinEffects(op.Effects) + " }"
				}
				line += " }"
				p.field("        ", line, op.Pos.Line)
			}
			p.blockTail(d.Operations.Pos.Line, "        ")
			p.WriteString("    }\n")
		}
	}
	p.blockTail(d.Pos.Line, "    ")
	p.WriteString("}\n")
}

// printStringLitField prints a quoted string field only when the literal is present (unlike
// printStringField, which always emits). Used for optional transport fields.
func printStringLitField(p *printer, indent, name string, s *StringLit) {
	if s == nil {
		return
	}
	printStringField(p, indent, name, s.Value, s.Pos.Line)
}

// printHeadersBlock renders a `headers { "<key>" "<value>" … }` block in author order.
func printHeadersBlock(p *printer, indent string, headers []*HeaderPair) {
	if len(headers) == 0 {
		return
	}
	fmt.Fprintf(p, "%sheaders {\n", indent)
	for _, h := range headers {
		if h == nil || h.Key == nil {
			continue
		}
		fmt.Fprintf(p, "%s    %s %s\n", indent, strconv.Quote(h.Key.Value), strconv.Quote(stringLitOrEmpty(h.Value)))
	}
	fmt.Fprintf(p, "%s}\n", indent)
}

func stringLitOrEmpty(s *StringLit) string {
	if s == nil {
		return ""
	}
	return s.Value
}

func printPolicy(p *printer, d *PolicyDecl) {
	fmt.Fprintf(p, "policy %s {\n", identName(d.Name))
	if d.Preset != nil {
		p.leadingBefore(d.Preset.Pos.Line, "    ")
		p.field("    ", "preset "+identName(d.Preset), d.Preset.Pos.Line)
	}
	if e := d.Execution; e != nil {
		p.leadingBefore(e.Pos.Line, "    ")
		p.WriteString("    execution {\n")
		if e.MaxTotalCostUsd != nil {
			fmt.Fprintf(p, "        maxTotalCostUsd %s\n", strconv.FormatFloat(*e.MaxTotalCostUsd, 'f', -1, 64))
		}
		if e.MaxWallClockSeconds != nil {
			fmt.Fprintf(p, "        maxWallClockSeconds %d\n", *e.MaxWallClockSeconds)
		}
		if e.MaxIterations != nil {
			fmt.Fprintf(p, "        maxIterations %d\n", *e.MaxIterations)
		}
		printBoolField(p, "        ", "requireStructuredOutput", e.RequireStructuredOutput)
		p.blockTail(e.Pos.Line, "        ")
		p.WriteString("    }\n")
	}
	if a := d.Approvals; a != nil {
		p.leadingBefore(a.Pos.Line, "    ")
		p.WriteString("    approvals {\n")
		if len(a.RequiredFor) > 0 {
			p.WriteString("        requiredFor {\n")
			for _, g := range a.RequiredFor {
				fmt.Fprintf(p, "            %s\n", grantPath(g))
			}
			p.WriteString("        }\n")
		}
		printBoolField(p, "        ", "requireAllTools", a.RequireAllTools)
		printBoolField(p, "        ", "permissive", a.Permissive)
		p.blockTail(a.Pos.Line, "        ")
		p.WriteString("    }\n")
	}
	if e := d.Effects; e != nil {
		p.leadingBefore(e.Pos.Line, "    ")
		p.WriteString("    effects {\n")
		if len(e.Permit) > 0 {
			p.leadingBefore(e.Permit[0].Pos.Line, "        ")
			p.field("        ", fmt.Sprintf("permit { %s }", joinEffects(e.Permit)), e.Permit[0].Pos.Line)
		}
		if len(e.PermitWithApproval) > 0 {
			p.leadingBefore(e.PermitWithApproval[0].Pos.Line, "        ")
			p.field("        ", fmt.Sprintf("permitWithApproval { %s }", joinEffects(e.PermitWithApproval)), e.PermitWithApproval[0].Pos.Line)
		}
		p.blockTail(e.Pos.Line, "        ")
		p.WriteString("    }\n")
	}
	if h := d.Hitl; h != nil {
		p.leadingBefore(h.Pos.Line, "    ")
		printHitlAt(p, "    ", h)
	}
	if t := d.Tools; t != nil {
		p.leadingBefore(t.Pos.Line, "    ")
		p.WriteString("    tools {\n")
		printBoolField(p, "        ", "forbidUnknownTools", t.ForbidUnknownTools)
		p.blockTail(t.Pos.Line, "        ")
		p.WriteString("    }\n")
	}
	p.blockTail(d.Pos.Line, "    ")
	p.WriteString("}\n")
}

// printHitlAt renders a `hitl { … }` block at the given indent (issues #106, #440). interruptOn
// entries and switch-map entries print in author order (they are stored as slices), so a formatted
// file round-trips; the fixed-name config fields print in a stable order.
func printHitlAt(p *printer, indent string, h *HitlBlock) {
	inner := indent + "    "
	fmt.Fprintf(p, "%shitl {\n", indent)
	printStringLitField(p, inner, "descriptionPrefix", h.DescriptionPrefix)
	printStringListInline(p, inner, "redactKeys", h.RedactKeys)
	printSwitchMapBlock(p, inner, "toolSwitchMap", h.ToolSwitchMap)
	if len(h.InterruptOn) > 0 {
		fmt.Fprintf(p, "%sinterruptOn {\n", inner)
		entryIndent := inner + "    "
		for _, e := range h.InterruptOn {
			if e == nil || e.Name == nil {
				continue
			}
			if e.Config == nil {
				fmt.Fprintf(p, "%s%s\n", entryIndent, identName(e.Name))
				continue
			}
			fmt.Fprintf(p, "%s%s {\n", entryIndent, identName(e.Name))
			printInterruptConfig(p, entryIndent+"    ", e.Config)
			fmt.Fprintf(p, "%s}\n", entryIndent)
		}
		fmt.Fprintf(p, "%s}\n", inner)
	}
	p.blockTail(h.Pos.Line, inner)
	fmt.Fprintf(p, "%s}\n", indent)
}

// printInterruptConfig renders a per-tool interruptOn config block's fields in a stable order.
func printInterruptConfig(p *printer, indent string, c *InterruptConfig) {
	if len(c.AllowedDecisions) > 0 {
		names := make([]string, 0, len(c.AllowedDecisions))
		for _, d := range c.AllowedDecisions {
			if d != nil {
				names = append(names, d.Name)
			}
		}
		fmt.Fprintf(p, "%sallowedDecisions { %s }\n", indent, strings.Join(names, " "))
	}
	printStringLitField(p, indent, "description", c.Description)
	printStringListInline(p, indent, "allowedEditArgs", c.AllowedEditArgs)
	printStringListInline(p, indent, "deniedEditArgs", c.DeniedEditArgs)
	printStringListInline(p, indent, "allowedEditPaths", c.AllowedEditPaths)
	printStringListInline(p, indent, "deniedEditPaths", c.DeniedEditPaths)
	printStringListInline(p, indent, "allowedEditTools", c.AllowedEditTools)
	printSwitchMapBlock(p, indent, "switchMap", c.SwitchMap)
	printStringListInline(p, indent, "redactKeys", c.RedactKeys)
}

// printStringListInline renders `<name> { "a" "b" … }` on one line, skipping an empty list.
func printStringListInline(p *printer, indent, name string, items []*StringLit) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(p, "%s%s {", indent, name)
	for _, s := range items {
		fmt.Fprintf(p, " %s", strconv.Quote(stringLitOrEmpty(s)))
	}
	p.WriteString(" }\n")
}

// printSwitchMapBlock renders `<name> { <source> { <target> … } … }`, skipping an empty map.
func printSwitchMapBlock(p *printer, indent, name string, entries []*SwitchMapEntry) {
	if len(entries) == 0 {
		return
	}
	fmt.Fprintf(p, "%s%s {\n", indent, name)
	inner := indent + "    "
	for _, e := range entries {
		if e == nil || e.Source == nil {
			continue
		}
		fmt.Fprintf(p, "%s%s {", inner, identName(e.Source))
		for _, t := range e.Targets {
			if t != nil {
				fmt.Fprintf(p, " %s", identName(t))
			}
		}
		p.WriteString(" }\n")
	}
	fmt.Fprintf(p, "%s}\n", indent)
}

func printBoolField(p *printer, indent, name string, v *bool) {
	if v != nil {
		fmt.Fprintf(p, "%s%s %t\n", indent, name, *v)
	}
}

func printIntPtr(p *printer, indent, name string, v *int) {
	if v != nil {
		fmt.Fprintf(p, "%s%s %d\n", indent, name, *v)
	}
}

func printIdentField(p *printer, indent, name string, v *Ident) {
	if v != nil {
		fmt.Fprintf(p, "%s%s %s\n", indent, name, identName(v))
	}
}

func joinEffects(effs []*EffectRef) string {
	parts := make([]string, 0, len(effs))
	for _, e := range effs {
		if e != nil {
			parts = append(parts, e.Name)
		}
	}
	return strings.Join(parts, ", ")
}

// grantPath reprints a grant's full dotted path (tool.<name>.<operation>).
func grantPath(g *Grant) string {
	if g == nil {
		return ""
	}
	segs := make([]string, 0, len(g.Segments))
	for _, s := range g.Segments {
		segs = append(segs, s.Name)
	}
	return strings.Join(segs, ".")
}

// printEnvironment renders `environment <Name> { overrides { agents { … } policies { … } } }`
// (issue #440) with indent-parameterized sub-block helpers so the nested structure round-trips.
func printEnvironment(p *printer, d *EnvironmentDecl) {
	fmt.Fprintf(p, "environment %s {\n", identName(d.Name))
	if ov := d.Overrides; ov != nil {
		p.leadingBefore(ov.Pos.Line, "    ")
		p.WriteString("    overrides {\n")
		if len(ov.Agents) > 0 {
			p.WriteString("        agents {\n")
			for _, a := range ov.Agents {
				p.leadingBefore(a.Pos.Line, "            ")
				fmt.Fprintf(p, "            %s {\n", identName(a.Name))
				if a.Model != nil {
					p.leadingBefore(a.Model.Pos.Line, "                ")
					p.field("                ", "model "+a.Model.Raw, a.Model.Pos.Line)
				}
				if a.Constraints != nil {
					p.leadingBefore(a.Constraints.Pos.Line, "                ")
					printConstraintsAt(p, "                ", a.Constraints)
				}
				p.blockTail(a.Pos.Line, "                ")
				p.WriteString("            }\n")
			}
			p.WriteString("        }\n")
		}
		if len(ov.Policies) > 0 {
			p.WriteString("        policies {\n")
			for _, pol := range ov.Policies {
				p.leadingBefore(pol.Pos.Line, "            ")
				fmt.Fprintf(p, "            %s {\n", identName(pol.Name))
				if pol.Execution != nil {
					p.leadingBefore(pol.Execution.Pos.Line, "                ")
					printExecutionAt(p, "                ", pol.Execution)
				}
				if pol.Approvals != nil {
					p.leadingBefore(pol.Approvals.Pos.Line, "                ")
					printApprovalsAt(p, "                ", pol.Approvals)
				}
				p.blockTail(pol.Pos.Line, "                ")
				p.WriteString("            }\n")
			}
			p.WriteString("        }\n")
		}
		p.blockTail(ov.Pos.Line, "        ")
		p.WriteString("    }\n")
	}
	p.blockTail(d.Pos.Line, "    ")
	p.WriteString("}\n")
}

// printProvider renders `provider <alias> { type … apiKeyFrom "…" workspaceIdFrom "…" }` (issue #440).
func printProvider(p *printer, d *ProviderDecl) {
	fmt.Fprintf(p, "provider %s {\n", identName(d.Name))
	if d.Type != nil {
		p.leadingBefore(d.Type.Pos.Line, "    ")
		p.field("    ", "type "+identName(d.Type), d.Type.Pos.Line)
	}
	if d.APIKeyFrom != nil {
		p.leadingBefore(d.APIKeyFrom.Pos.Line, "    ")
	}
	printStringLitField(p, "    ", "apiKeyFrom", d.APIKeyFrom)
	if d.WorkspaceIDFrom != nil {
		p.leadingBefore(d.WorkspaceIDFrom.Pos.Line, "    ")
	}
	printStringLitField(p, "    ", "workspaceIdFrom", d.WorkspaceIDFrom)
	p.blockTail(d.Pos.Line, "    ")
	p.WriteString("}\n")
}

// printLimitsBlockAt renders a `limits { … }` block at the given indent (fields at indent+4), shared
// by the per-tool override and the top-level project baseline (issue #440).
func printLimitsBlockAt(p *printer, indent string, lim *ToolLimitsBlock) {
	inner := indent + "    "
	fmt.Fprintf(p, "%slimits {\n", indent)
	printIntPtr(p, inner, "maxToolInputBytes", lim.MaxToolInputBytes)
	printIntPtr(p, inner, "maxToolOutputBytes", lim.MaxToolOutputBytes)
	printIntPtr(p, inner, "maxCheckpointBytes", lim.MaxCheckpointBytes)
	printIntPtr(p, inner, "maxStateBytes", lim.MaxStateBytes)
	printIntPtr(p, inner, "maxWorkflowNesting", lim.MaxWorkflowNesting)
	printIntPtr(p, inner, "maxLoopIterations", lim.MaxLoopIterations)
	printIdentField(p, inner, "toolInputExceedPolicy", lim.ToolInputExceedPolicy)
	printIdentField(p, inner, "toolOutputExceedPolicy", lim.ToolOutputExceedPolicy)
	printIdentField(p, inner, "checkpointExceedPolicy", lim.CheckpointExceedPolicy)
	p.blockTail(lim.Pos.Line, inner)
	fmt.Fprintf(p, "%s}\n", indent)
}

// printLimitsDecl renders the top-level singleton `limits { … }` declaration (issue #440, ADR 007).
func printLimitsDecl(p *printer, d *LimitsDecl) {
	if d.Block == nil {
		p.WriteString("limits {\n}\n")
		return
	}
	printLimitsBlockAt(p, "", d.Block)
}

// printDefaults renders the singleton `defaults { policy … model … runtime … }` block (issue #440).
// Fields absent from the AST are omitted, so an empty block prints as `defaults {\n}`.
func printDefaults(p *printer, d *DefaultsDecl) {
	p.WriteString("defaults {\n")
	if d.Policy != nil {
		p.leadingBefore(d.Policy.Pos.Line, "    ")
		p.field("    ", "policy "+identName(d.Policy), d.Policy.Pos.Line)
	}
	if d.Model != nil {
		p.leadingBefore(d.Model.Pos.Line, "    ")
		p.field("    ", fmt.Sprintf("model %s/%s", d.Model.Provider, d.Model.Name), d.Model.Pos.Line)
	}
	if d.Runtime != nil {
		p.leadingBefore(d.Runtime.Pos.Line, "    ")
		p.field("    ", "runtime "+identName(d.Runtime), d.Runtime.Pos.Line)
	}
	p.blockTail(d.Pos.Line, "    ")
	p.WriteString("}\n")
}

// printConstraintsAt renders a `constraints { … }` block at the given indent (fields at indent+4).
func printConstraintsAt(p *printer, indent string, c *Constraints) {
	inner := indent + "    "
	fmt.Fprintf(p, "%sconstraints {\n", indent)
	if c.MaxIterations != nil {
		fmt.Fprintf(p, "%smaxIterations %d\n", inner, *c.MaxIterations)
	}
	if c.MaxTokens != nil {
		fmt.Fprintf(p, "%smaxTokens %d\n", inner, *c.MaxTokens)
	}
	if c.TimeoutSeconds != nil {
		fmt.Fprintf(p, "%stimeoutSeconds %d\n", inner, *c.TimeoutSeconds)
	}
	if c.Temperature != nil {
		fmt.Fprintf(p, "%stemperature %s\n", inner, strconv.FormatFloat(*c.Temperature, 'g', -1, 64))
	}
	if c.RequireStructuredOutput != nil {
		fmt.Fprintf(p, "%srequireStructuredOutput %s\n", inner, strconv.FormatBool(*c.RequireStructuredOutput))
	}
	p.blockTail(c.Pos.Line, inner)
	fmt.Fprintf(p, "%s}\n", indent)
}

// printExecutionAt renders an `execution { … }` block at the given indent.
func printExecutionAt(p *printer, indent string, e *PolicyExecutionBlock) {
	inner := indent + "    "
	fmt.Fprintf(p, "%sexecution {\n", indent)
	if e.MaxTotalCostUsd != nil {
		fmt.Fprintf(p, "%smaxTotalCostUsd %s\n", inner, strconv.FormatFloat(*e.MaxTotalCostUsd, 'f', -1, 64))
	}
	if e.MaxWallClockSeconds != nil {
		fmt.Fprintf(p, "%smaxWallClockSeconds %d\n", inner, *e.MaxWallClockSeconds)
	}
	if e.MaxIterations != nil {
		fmt.Fprintf(p, "%smaxIterations %d\n", inner, *e.MaxIterations)
	}
	printBoolField(p, inner, "requireStructuredOutput", e.RequireStructuredOutput)
	p.blockTail(e.Pos.Line, inner)
	fmt.Fprintf(p, "%s}\n", indent)
}

// printApprovalsAt renders an `approvals { … }` block at the given indent.
func printApprovalsAt(p *printer, indent string, a *PolicyApprovalsBlock) {
	inner := indent + "    "
	fmt.Fprintf(p, "%sapprovals {\n", indent)
	if len(a.RequiredFor) > 0 {
		fmt.Fprintf(p, "%srequiredFor {\n", inner)
		for _, g := range a.RequiredFor {
			fmt.Fprintf(p, "%s    %s\n", inner, grantPath(g))
		}
		fmt.Fprintf(p, "%s}\n", inner)
	}
	printBoolField(p, inner, "requireAllTools", a.RequireAllTools)
	printBoolField(p, inner, "permissive", a.Permissive)
	p.blockTail(a.Pos.Line, inner)
	fmt.Fprintf(p, "%s}\n", indent)
}
