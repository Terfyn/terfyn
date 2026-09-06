package lang

import "strings"

// isResourceDeclKeyword reports whether the current token opens a top-level `tool`/`policy`
// declaration. `tool` and `policy` are contextual: they are ordinary identifiers elsewhere (a grant
// path `tool.x.y`, an agent field `policy foo`), and only at the top level introduce a declaration.
func (p *parser) isResourceDeclKeyword() bool {
	return p.cur.Kind == KindIdent && (p.cur.Lit == "tool" || p.cur.Lit == "policy" || p.cur.Lit == "environment" || p.cur.Lit == "provider" || p.cur.Lit == "defaults" || p.cur.Lit == "limits")
}

// parseTool parses `tool <Name> { type … safety { … } operations { … } }` (ADR 005).
func (p *parser) parseTool() *ToolDecl {
	d := &ToolDecl{Pos: p.cur.Pos}
	p.advance() // consume 'tool'
	d.Name = p.ident("after 'tool'")
	if _, ok := p.expect(KindLBrace, "to open tool body"); !ok {
		return d
	}
	seen := map[string]bool{}
	dup := func(field string, pos Pos) bool {
		if seen[field] {
			p.errorf(pos, "duplicate tool field %q (each field may appear at most once)", field)
			return true
		}
		seen[field] = true
		return false
	}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected tool field (type, mcp, http, workspace, safety, operations), got %s", p.cur)
			p.syncLine()
			continue
		}
		field, fpos := p.cur.Lit, p.cur.Pos
		switch field {
		case "type":
			p.advance()
			if id := p.ident("after 'type'"); !dup(field, fpos) {
				d.Type = id
			}
		case "mcp":
			p.advance()
			if b := p.parseToolMCP(); !dup(field, fpos) {
				d.MCP = b
			}
		case "http":
			p.advance()
			if b := p.parseToolHTTP(); !dup(field, fpos) {
				d.HTTP = b
			}
		case "workspace":
			p.advance()
			if b := p.parseToolWorkspace(); !dup(field, fpos) {
				d.Workspace = b
			}
		case "retry":
			p.advance()
			if b := p.parseToolRetry(); !dup(field, fpos) {
				d.Retry = b
			}
		case "limits":
			p.advance()
			if b := p.parseToolLimits(); !dup(field, fpos) {
				d.Limits = b
			}
		case "safety":
			p.advance()
			if b := p.parseToolSafety(); !dup(field, fpos) {
				d.Safety = b
			}
		case "operations":
			p.advance()
			if ops := p.parseToolOperations(); !dup(field, fpos) {
				d.Operations = ops
			}
		default:
			p.errorf(fpos, "unknown tool field %q (want type, mcp, http, workspace, retry, limits, safety, or operations)", field)
			p.syncLine()
		}
	}
	p.expect(KindRBrace, "to close tool body")
	return d
}

// parseLimitsDecl parses the top-level singleton `limits { … }` declaration (issue #440, ADR 007),
// reusing parseToolLimits for the shared nine-field body. The keyword position is captured before the
// body is parsed so a duplicate/collision diagnostic can point at the block.
func (p *parser) parseLimitsDecl() *LimitsDecl {
	d := &LimitsDecl{Pos: p.cur.Pos}
	p.advance() // consume 'limits'
	d.Block = p.parseToolLimits()
	return d
}

// parseToolLimits parses `limits { maxToolInputBytes N … toolInputExceedPolicy <truncate|fail> … }`
// (issue #440) — per-tool execution-limit overrides. Byte/count fields are ints; the three
// *ExceedPolicy fields are the `truncate` / `fail` enum.
func (p *parser) parseToolLimits() *ToolLimitsBlock {
	b := &ToolLimitsBlock{Pos: p.cur.Pos}
	if _, ok := p.expect(KindLBrace, "to open limits block"); !ok {
		return b
	}
	seen := map[string]bool{}
	intField := func() *int {
		if v, ok := p.constraintInt("limit"); ok {
			return &v
		}
		return nil
	}
	policyField := func() *Ident {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected an exceed policy (truncate or fail), got %s", p.cur)
			return nil
		}
		switch p.cur.Lit {
		case "truncate", "fail":
			id := &Ident{Pos: p.cur.Pos, Name: p.cur.Lit}
			p.advance()
			return id
		default:
			p.errorf(p.cur.Pos, "unknown exceed policy %q (want truncate or fail)", p.cur.Lit)
			p.advance()
			return nil
		}
	}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected a limits field, got %s", p.cur)
			p.syncLine()
			continue
		}
		field, fpos := p.cur.Lit, p.cur.Pos
		p.advance()
		if seen[field] {
			p.errorf(fpos, "duplicate limits field %q", field)
		}
		seen[field] = true
		switch field {
		case "maxToolInputBytes":
			b.MaxToolInputBytes = intField()
		case "maxToolOutputBytes":
			b.MaxToolOutputBytes = intField()
		case "maxCheckpointBytes":
			b.MaxCheckpointBytes = intField()
		case "maxStateBytes":
			b.MaxStateBytes = intField()
		case "maxWorkflowNesting":
			b.MaxWorkflowNesting = intField()
		case "maxLoopIterations":
			b.MaxLoopIterations = intField()
		case "toolInputExceedPolicy":
			b.ToolInputExceedPolicy = policyField()
		case "toolOutputExceedPolicy":
			b.ToolOutputExceedPolicy = policyField()
		case "checkpointExceedPolicy":
			b.CheckpointExceedPolicy = policyField()
		default:
			p.errorf(fpos, "unknown limits field %q", field)
			p.syncLine()
		}
	}
	p.expect(KindRBrace, "to close limits block")
	return b
}

// parseToolRetry parses `retry { maxAttempts N backoff "…" }` (issue #440). Both fields optional.
func (p *parser) parseToolRetry() *ToolRetryBlock {
	b := &ToolRetryBlock{Pos: p.cur.Pos}
	if _, ok := p.expect(KindLBrace, "to open retry block"); !ok {
		return b
	}
	seen := map[string]bool{}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected a retry field (maxAttempts, backoff), got %s", p.cur)
			p.syncLine()
			continue
		}
		field, fpos := p.cur.Lit, p.cur.Pos
		p.advance()
		if seen[field] {
			p.errorf(fpos, "duplicate retry field %q", field)
		}
		seen[field] = true
		switch field {
		case "maxAttempts":
			if v, ok := p.constraintInt(field); ok {
				b.MaxAttempts = &v
			}
		case "backoff":
			b.Backoff = p.parseStringLit("for backoff")
		default:
			p.errorf(fpos, "unknown retry field %q (want maxAttempts or backoff)", field)
			p.syncLine()
		}
	}
	p.expect(KindRBrace, "to close retry block")
	return b
}

// parseToolWorkspace parses `workspace { root "…" testCommand "…" }` — declarative native workspace
// config (issue #440). Both fields are optional string literals; the env fallback applies when absent.
func (p *parser) parseToolWorkspace() *ToolWorkspaceBlock {
	b := &ToolWorkspaceBlock{Pos: p.cur.Pos}
	if _, ok := p.expect(KindLBrace, "to open workspace block"); !ok {
		return b
	}
	seen := map[string]bool{}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected a workspace field (root, testCommand), got %s", p.cur)
			p.syncLine()
			continue
		}
		field, fpos := p.cur.Lit, p.cur.Pos
		p.advance()
		if seen[field] {
			p.errorf(fpos, "duplicate workspace field %q", field)
		}
		seen[field] = true
		switch field {
		case "root":
			b.Root = p.parseStringLit("for root")
		case "testCommand":
			b.TestCommand = p.parseStringLit("for testCommand")
		default:
			p.errorf(fpos, "unknown workspace field %q (want root or testCommand)", field)
			p.syncLine()
		}
	}
	p.expect(KindRBrace, "to close workspace block")
	return b
}

func (p *parser) parseToolSafety() *ToolSafetyBlock {
	b := &ToolSafetyBlock{Pos: p.cur.Pos}
	if _, ok := p.expect(KindLBrace, "to open safety block"); !ok {
		return b
	}
	seen := map[string]bool{}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected a safety field (trusted, sideEffects, requiresApproval), got %s", p.cur)
			p.syncLine()
			continue
		}
		field, fpos := p.cur.Lit, p.cur.Pos
		p.advance()
		if seen[field] {
			p.errorf(fpos, "duplicate safety field %q", field)
		}
		seen[field] = true
		switch field {
		case "trusted":
			if v, ok := p.constraintBool(field); ok {
				b.Trusted = &v
			}
		case "sideEffects":
			if v, ok := p.constraintBool(field); ok {
				b.SideEffects = &v
			}
		case "requiresApproval":
			if v, ok := p.constraintBool(field); ok {
				b.RequiresApproval = &v
			}
		default:
			p.errorf(fpos, "unknown safety field %q (want trusted, sideEffects, or requiresApproval)", field)
			p.syncLine()
		}
	}
	p.expect(KindRBrace, "to close safety block")
	return b
}

// parseToolMCP parses `mcp { transport … command … args { … } url … headers { … } }` (issue #440).
func (p *parser) parseToolMCP() *ToolMCPBlock {
	b := &ToolMCPBlock{Pos: p.cur.Pos}
	if _, ok := p.expect(KindLBrace, "to open mcp block"); !ok {
		return b
	}
	seen := map[string]bool{}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected an mcp field (transport, command, args, url, headers), got %s", p.cur)
			p.syncLine()
			continue
		}
		field, fpos := p.cur.Lit, p.cur.Pos
		p.advance()
		if seen[field] {
			p.errorf(fpos, "duplicate mcp field %q", field)
		}
		seen[field] = true
		switch field {
		case "transport":
			b.Transport = p.parseStringLit("after 'transport'")
		case "command":
			b.Command = p.parseStringLit("after 'command'")
		case "url":
			b.URL = p.parseStringLit("after 'url'")
		case "args":
			b.Args = p.parseStringListBlock("args")
		case "headers":
			b.Headers = p.parseHeadersBlock()
		default:
			p.errorf(fpos, "unknown mcp field %q (want transport, command, args, url, or headers)", field)
			p.syncLine()
		}
	}
	p.expect(KindRBrace, "to close mcp block")
	return b
}

// parseToolHTTP parses `http { baseUrl … headers { … } }` (issue #440).
func (p *parser) parseToolHTTP() *ToolHTTPBlock {
	b := &ToolHTTPBlock{Pos: p.cur.Pos}
	if _, ok := p.expect(KindLBrace, "to open http block"); !ok {
		return b
	}
	seen := map[string]bool{}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected an http field (baseUrl, headers), got %s", p.cur)
			p.syncLine()
			continue
		}
		field, fpos := p.cur.Lit, p.cur.Pos
		p.advance()
		if seen[field] {
			p.errorf(fpos, "duplicate http field %q", field)
		}
		seen[field] = true
		switch field {
		case "baseUrl":
			b.BaseURL = p.parseStringLit("after 'baseUrl'")
		case "headers":
			b.Headers = p.parseHeadersBlock()
		default:
			p.errorf(fpos, "unknown http field %q (want baseUrl or headers)", field)
			p.syncLine()
		}
	}
	p.expect(KindRBrace, "to close http block")
	return b
}

// parseStringListBlock parses `<field> { "a" "b" … }` — a whitespace-separated list of string
// literals (e.g. mcp args). Order is preserved.
func (p *parser) parseStringListBlock(field string) []*StringLit {
	if _, ok := p.expect(KindLBrace, "to open "+field+" block"); !ok {
		return nil
	}
	var out []*StringLit
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindString {
			p.errorf(p.cur.Pos, "expected a string in %s { … }, got %s", field, p.cur)
			p.syncLine()
			continue
		}
		if s := p.parseStringLit("in " + field); s != nil {
			out = append(out, s)
		}
	}
	p.expect(KindRBrace, "to close "+field+" block")
	return out
}

// parseHeadersBlock parses `headers { "<key>" "<value>" … }` — string key/value pairs.
func (p *parser) parseHeadersBlock() []*HeaderPair {
	if _, ok := p.expect(KindLBrace, "to open headers block"); !ok {
		return nil
	}
	var out []*HeaderPair
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindString {
			p.errorf(p.cur.Pos, "expected a header key string in headers { … }, got %s", p.cur)
			p.syncLine()
			continue
		}
		pos := p.cur.Pos
		key := p.parseStringLit("for the header key")
		if p.cur.Kind != KindString {
			p.errorf(p.cur.Pos, "expected a value string after header key, got %s", p.cur)
			p.syncLine()
			continue
		}
		val := p.parseStringLit("for the header value")
		out = append(out, &HeaderPair{Pos: pos, Key: key, Value: val})
	}
	p.expect(KindRBrace, "to close headers block")
	return out
}

func (p *parser) parseToolOperations() *ToolOperations {
	ops := &ToolOperations{Pos: p.cur.Pos}
	if _, ok := p.expect(KindLBrace, "to open operations block"); !ok {
		return ops
	}
	seen := map[string]bool{}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected an operation name, got %s", p.cur)
			p.syncLine()
			continue
		}
		op := p.parseToolOperation()
		if op != nil {
			name := identName(op.Name)
			if seen[name] {
				p.errorf(op.Pos, "duplicate operation %q", name)
			}
			seen[name] = true
			ops.Ops = append(ops.Ops, op)
		}
	}
	p.expect(KindRBrace, "to close operations block")
	return ops
}

// parseToolOperation parses `<op> { [effects { … }] }`. The op name may be dotted
// (pull_request.post_comment); its dotted form is joined into a single Ident.
func (p *parser) parseToolOperation() *ToolOperationDecl {
	parts := p.parseDottedPath("operation name")
	if len(parts) == 0 {
		p.syncLine()
		return nil
	}
	names := make([]string, len(parts))
	for i, pt := range parts {
		names[i] = pt.Name
	}
	op := &ToolOperationDecl{Pos: parts[0].Pos, Name: &Ident{Pos: parts[0].Pos, Name: strings.Join(names, ".")}}
	if _, ok := p.expect(KindLBrace, "to open operation body"); !ok {
		return op
	}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind == KindIdent && p.cur.Lit == "effects" {
			p.advance()
			op.Effects = p.parseEffects()
			continue
		}
		if p.cur.Kind == KindIdent && p.cur.Lit == "schema" {
			p.advance()
			op.Schema = p.parseStringLit("for operation schema")
			continue
		}
		p.errorf(p.cur.Pos, "expected 'schema' or 'effects' in operation body, got %s", p.cur)
		p.syncLine()
	}
	p.expect(KindRBrace, "to close operation body")
	return op
}

// parsePolicy parses `policy <Name> { execution { … } approvals { … } effects { … } }`.
func (p *parser) parsePolicy() *PolicyDecl {
	d := &PolicyDecl{Pos: p.cur.Pos}
	p.advance() // consume 'policy'
	d.Name = p.ident("after 'policy'")
	if _, ok := p.expect(KindLBrace, "to open policy body"); !ok {
		return d
	}
	seen := map[string]bool{}
	dup := func(field string, pos Pos) bool {
		if seen[field] {
			p.errorf(pos, "duplicate policy field %q (each field may appear at most once)", field)
			return true
		}
		seen[field] = true
		return false
	}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected policy field (preset, execution, approvals, effects, hitl), got %s", p.cur)
			p.syncLine()
			continue
		}
		field, fpos := p.cur.Lit, p.cur.Pos
		switch field {
		case "preset":
			p.advance()
			if id := p.ident("after 'preset'"); !dup(field, fpos) {
				d.Preset = id
			}
		case "execution":
			p.advance()
			if b := p.parsePolicyExecution(); !dup(field, fpos) {
				d.Execution = b
			}
		case "approvals":
			p.advance()
			if b := p.parsePolicyApprovals(); !dup(field, fpos) {
				d.Approvals = b
			}
		case "effects":
			p.advance()
			if b := p.parsePolicyEffects(); !dup(field, fpos) {
				d.Effects = b
			}
		case "hitl":
			p.advance()
			if b := p.parsePolicyHitl(); !dup(field, fpos) {
				d.Hitl = b
			}
		case "tools":
			p.advance()
			if b := p.parsePolicyTools(); !dup(field, fpos) {
				d.Tools = b
			}
		default:
			p.errorf(fpos, "unknown policy field %q (want preset, execution, approvals, effects, hitl, or tools)", field)
			p.syncLine()
		}
	}
	p.expect(KindRBrace, "to close policy body")
	return d
}

func (p *parser) parsePolicyExecution() *PolicyExecutionBlock {
	b := &PolicyExecutionBlock{Pos: p.cur.Pos}
	if _, ok := p.expect(KindLBrace, "to open execution block"); !ok {
		return b
	}
	seen := map[string]bool{}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected an execution field, got %s", p.cur)
			p.syncLine()
			continue
		}
		field, fpos := p.cur.Lit, p.cur.Pos
		p.advance()
		if seen[field] {
			p.errorf(fpos, "duplicate execution field %q", field)
		}
		seen[field] = true
		switch field {
		case "maxTotalCostUsd":
			if v, ok := p.constraintFloat(field); ok {
				b.MaxTotalCostUsd = &v
			}
		case "maxWallClockSeconds":
			if v, ok := p.constraintInt(field); ok {
				b.MaxWallClockSeconds = &v
			}
		case "maxIterations":
			if v, ok := p.constraintInt(field); ok {
				b.MaxIterations = &v
			}
		case "requireStructuredOutput":
			if v, ok := p.constraintBool(field); ok {
				b.RequireStructuredOutput = &v
			}
		default:
			p.errorf(fpos, "unknown execution field %q (want maxTotalCostUsd, maxWallClockSeconds, maxIterations, or requireStructuredOutput)", field)
			p.syncLine()
		}
	}
	p.expect(KindRBrace, "to close execution block")
	return b
}

func (p *parser) parsePolicyApprovals() *PolicyApprovalsBlock {
	b := &PolicyApprovalsBlock{Pos: p.cur.Pos}
	if _, ok := p.expect(KindLBrace, "to open approvals block"); !ok {
		return b
	}
	seen := map[string]bool{}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected an approvals field (requiredFor, requireAllTools, permissive), got %s", p.cur)
			p.syncLine()
			continue
		}
		field, fpos := p.cur.Lit, p.cur.Pos
		p.advance()
		if seen[field] {
			p.errorf(fpos, "duplicate approvals field %q", field)
		}
		seen[field] = true
		switch field {
		case "requiredFor":
			b.RequiredFor = p.parseGrants()
		case "requireAllTools":
			if v, ok := p.constraintBool(field); ok {
				b.RequireAllTools = &v
			}
		case "permissive":
			if v, ok := p.constraintBool(field); ok {
				b.Permissive = &v
			}
		default:
			p.errorf(fpos, "unknown approvals field %q (want requiredFor, requireAllTools, or permissive)", field)
			p.syncLine()
		}
	}
	p.expect(KindRBrace, "to close approvals block")
	return b
}

func (p *parser) parsePolicyEffects() *PolicyEffectsBlock {
	b := &PolicyEffectsBlock{Pos: p.cur.Pos}
	if _, ok := p.expect(KindLBrace, "to open effects block"); !ok {
		return b
	}
	seen := map[string]bool{}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent || (p.cur.Lit != "permit" && p.cur.Lit != "permitWithApproval") {
			p.errorf(p.cur.Pos, "expected 'permit' or 'permitWithApproval', got %s", p.cur)
			p.syncLine()
			continue
		}
		field, fpos := p.cur.Lit, p.cur.Pos
		p.advance()
		if seen[field] {
			p.errorf(fpos, "duplicate %q block", field)
		}
		seen[field] = true
		if field == "permit" {
			b.Permit = p.parseEffects()
		} else {
			b.PermitWithApproval = p.parseEffects()
		}
	}
	p.expect(KindRBrace, "to close effects block")
	return b
}

// parsePolicyTools parses `tools { forbidUnknownTools <bool> }` (issue #440).
func (p *parser) parsePolicyTools() *PolicyToolsBlock {
	b := &PolicyToolsBlock{Pos: p.cur.Pos}
	if _, ok := p.expect(KindLBrace, "to open tools block"); !ok {
		return b
	}
	seen := map[string]bool{}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected a tools field (forbidUnknownTools), got %s", p.cur)
			p.syncLine()
			continue
		}
		field, fpos := p.cur.Lit, p.cur.Pos
		p.advance()
		if seen[field] {
			p.errorf(fpos, "duplicate tools field %q", field)
		}
		seen[field] = true
		switch field {
		case "forbidUnknownTools":
			if v, ok := p.constraintBool(field); ok {
				b.ForbidUnknownTools = &v
			}
		default:
			p.errorf(fpos, "unknown tools field %q (want forbidUnknownTools)", field)
			p.syncLine()
		}
	}
	p.expect(KindRBrace, "to close tools block")
	return b
}

// parsePolicyHitl parses `hitl { descriptionPrefix … redactKeys { … } toolSwitchMap { … }
// interruptOn { … } }` (issues #106, #440).
func (p *parser) parsePolicyHitl() *HitlBlock {
	b := &HitlBlock{Pos: p.cur.Pos}
	if _, ok := p.expect(KindLBrace, "to open hitl block"); !ok {
		return b
	}
	seen := map[string]bool{}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected a hitl field (descriptionPrefix, redactKeys, toolSwitchMap, interruptOn), got %s", p.cur)
			p.syncLine()
			continue
		}
		field, fpos := p.cur.Lit, p.cur.Pos
		p.advance()
		if seen[field] {
			p.errorf(fpos, "duplicate hitl field %q", field)
		}
		seen[field] = true
		switch field {
		case "descriptionPrefix":
			b.DescriptionPrefix = p.parseStringLit("for descriptionPrefix")
		case "redactKeys":
			b.RedactKeys = p.parseStringListBlock("redactKeys")
		case "toolSwitchMap":
			b.ToolSwitchMap = p.parseSwitchMapBlock("toolSwitchMap")
		case "interruptOn":
			b.InterruptOn = p.parseInterruptOnBlock()
		default:
			p.errorf(fpos, "unknown hitl field %q (want descriptionPrefix, redactKeys, toolSwitchMap, or interruptOn)", field)
			p.syncLine()
		}
	}
	p.expect(KindRBrace, "to close hitl block")
	return b
}

// parseSwitchMapBlock parses `<field> { <source-op> { <target-op> … } … }` — a map from a source
// operation to its allowed switch targets. Operation names may be dotted (joined into one Ident).
func (p *parser) parseSwitchMapBlock(field string) []*SwitchMapEntry {
	if _, ok := p.expect(KindLBrace, "to open "+field+" block"); !ok {
		return nil
	}
	var out []*SwitchMapEntry
	seen := map[string]bool{}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected a source operation in %s { … }, got %s", field, p.cur)
			p.syncLine()
			continue
		}
		src := p.parseDottedPath("source operation")
		if len(src) == 0 {
			p.syncLine()
			continue
		}
		e := &SwitchMapEntry{Pos: src[0].Pos, Source: &Ident{Pos: src[0].Pos, Name: dottedName(src)}}
		if seen[e.Source.Name] {
			p.errorf(e.Pos, "duplicate %s source %q", field, e.Source.Name)
		}
		seen[e.Source.Name] = true
		if _, ok := p.expect(KindLBrace, "to open the switch-target list"); !ok {
			continue
		}
		for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
			if p.cur.Kind != KindIdent {
				p.errorf(p.cur.Pos, "expected a target operation, got %s", p.cur)
				p.syncLine()
				continue
			}
			tgt := p.parseDottedPath("target operation")
			if len(tgt) == 0 {
				p.syncLine()
				continue
			}
			e.Targets = append(e.Targets, &Ident{Pos: tgt[0].Pos, Name: dottedName(tgt)})
		}
		p.expect(KindRBrace, "to close the switch-target list")
		out = append(out, e)
	}
	p.expect(KindRBrace, "to close "+field+" block")
	return out
}

// parseInterruptOnBlock parses `interruptOn { <tool> [ { … } ] … }`. A bare tool name is enabled
// with defaults (YAML `true`); a `<tool> { … }` supplies per-tool review config.
func (p *parser) parseInterruptOnBlock() []*InterruptEntry {
	if _, ok := p.expect(KindLBrace, "to open interruptOn block"); !ok {
		return nil
	}
	var out []*InterruptEntry
	seen := map[string]bool{}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected a tool name in interruptOn { … }, got %s", p.cur)
			p.syncLine()
			continue
		}
		name := p.ident("for an interruptOn tool")
		if name == nil {
			p.syncLine()
			continue
		}
		if seen[name.Name] {
			p.errorf(name.Pos, "duplicate interruptOn tool %q", name.Name)
		}
		seen[name.Name] = true
		e := &InterruptEntry{Pos: name.Pos, Name: name}
		if p.cur.Kind == KindLBrace {
			e.Config = p.parseInterruptConfig()
		}
		out = append(out, e)
	}
	p.expect(KindRBrace, "to close interruptOn block")
	return out
}

// parseInterruptConfig parses a per-tool `{ allowedDecisions { … } description … allowedEditArgs { … }
// … switchMap { … } redactKeys { … } }` review block.
func (p *parser) parseInterruptConfig() *InterruptConfig {
	c := &InterruptConfig{Pos: p.cur.Pos}
	if _, ok := p.expect(KindLBrace, "to open interruptOn config"); !ok {
		return c
	}
	seen := map[string]bool{}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected an interruptOn config field, got %s", p.cur)
			p.syncLine()
			continue
		}
		field, fpos := p.cur.Lit, p.cur.Pos
		p.advance()
		if seen[field] {
			p.errorf(fpos, "duplicate interruptOn config field %q", field)
		}
		seen[field] = true
		switch field {
		case "allowedDecisions":
			c.AllowedDecisions = p.parseDecisionListBlock()
		case "description":
			c.Description = p.parseStringLit("for description")
		case "allowedEditArgs":
			c.AllowedEditArgs = p.parseStringListBlock("allowedEditArgs")
		case "deniedEditArgs":
			c.DeniedEditArgs = p.parseStringListBlock("deniedEditArgs")
		case "allowedEditPaths":
			c.AllowedEditPaths = p.parseStringListBlock("allowedEditPaths")
		case "deniedEditPaths":
			c.DeniedEditPaths = p.parseStringListBlock("deniedEditPaths")
		case "allowedEditTools":
			c.AllowedEditTools = p.parseStringListBlock("allowedEditTools")
		case "switchMap":
			c.SwitchMap = p.parseSwitchMapBlock("switchMap")
		case "redactKeys":
			c.RedactKeys = p.parseStringListBlock("redactKeys")
		default:
			p.errorf(fpos, "unknown interruptOn config field %q", field)
			p.syncLine()
		}
	}
	p.expect(KindRBrace, "to close interruptOn config")
	return c
}

// parseDecisionListBlock parses `allowedDecisions { approve reject edit switch }` — a list of the
// four decision kinds. Unknown kinds are diagnosed; validity is re-checked during lowering/check.
func (p *parser) parseDecisionListBlock() []*Ident {
	if _, ok := p.expect(KindLBrace, "to open allowedDecisions block"); !ok {
		return nil
	}
	var out []*Ident
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected a decision (approve, reject, edit, switch), got %s", p.cur)
			p.syncLine()
			continue
		}
		switch p.cur.Lit {
		case "approve", "reject", "edit", "switch":
			out = append(out, &Ident{Pos: p.cur.Pos, Name: p.cur.Lit})
			p.advance()
		default:
			p.errorf(p.cur.Pos, "unknown decision %q (want approve, reject, edit, or switch)", p.cur.Lit)
			p.advance()
		}
	}
	p.expect(KindRBrace, "to close allowedDecisions block")
	return out
}

// parseProvider parses `provider <alias> { type <ident> apiKeyFrom "…" workspaceIdFrom "…" }`
// (issue #440). `type` is required; the two credential references are optional string literals.
func (p *parser) parseProvider() *ProviderDecl {
	d := &ProviderDecl{Pos: p.cur.Pos}
	p.advance() // consume 'provider'
	d.Name = p.ident("after 'provider'")
	if _, ok := p.expect(KindLBrace, "to open provider body"); !ok {
		return d
	}
	seen := map[string]bool{}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected a provider field (type, apiKeyFrom, workspaceIdFrom), got %s", p.cur)
			p.syncLine()
			continue
		}
		field, fpos := p.cur.Lit, p.cur.Pos
		p.advance()
		if seen[field] {
			p.errorf(fpos, "duplicate provider field %q", field)
		}
		seen[field] = true
		switch field {
		case "type":
			d.Type = p.ident("after 'type'")
		case "apiKeyFrom":
			d.APIKeyFrom = p.parseStringLit("for apiKeyFrom")
		case "workspaceIdFrom":
			d.WorkspaceIDFrom = p.parseStringLit("for workspaceIdFrom")
		default:
			p.errorf(fpos, "unknown provider field %q (want type, apiKeyFrom, or workspaceIdFrom)", field)
			p.syncLine()
		}
	}
	p.expect(KindRBrace, "to close provider body")
	return d
}

// parseDefaults parses the singleton `defaults { policy <ident> model <provider>/<name> runtime
// <ident> }` (issue #440, ADR 007). Every field is optional; each may appear at most once.
func (p *parser) parseDefaults() *DefaultsDecl {
	d := &DefaultsDecl{Pos: p.cur.Pos}
	p.advance() // consume 'defaults'
	if _, ok := p.expect(KindLBrace, "to open defaults body"); !ok {
		return d
	}
	seen := map[string]bool{}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected a defaults field (policy, model, runtime), got %s", p.cur)
			p.syncLine()
			continue
		}
		field, fpos := p.cur.Lit, p.cur.Pos
		p.advance()
		if seen[field] {
			p.errorf(fpos, "duplicate defaults field %q", field)
		}
		seen[field] = true
		switch field {
		case "policy":
			d.Policy = p.ident("after 'policy'")
		case "model":
			d.Model = p.parseModelRef()
		case "runtime":
			d.Runtime = p.ident("after 'runtime'")
		default:
			p.errorf(fpos, "unknown defaults field %q (want policy, model, or runtime)", field)
			p.syncLine()
		}
	}
	p.expect(KindRBrace, "to close defaults body")
	return d
}

// parseEnvironment parses `environment <Name> { overrides { … } }` (issue #440).
func (p *parser) parseEnvironment() *EnvironmentDecl {
	d := &EnvironmentDecl{Pos: p.cur.Pos}
	p.advance() // consume 'environment'
	d.Name = p.ident("after 'environment'")
	if _, ok := p.expect(KindLBrace, "to open environment body"); !ok {
		return d
	}
	seen := map[string]bool{}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected an environment field (overrides), got %s", p.cur)
			p.syncLine()
			continue
		}
		field, fpos := p.cur.Lit, p.cur.Pos
		switch field {
		case "overrides":
			p.advance()
			if seen[field] {
				p.errorf(fpos, "duplicate environment field %q", field)
			}
			seen[field] = true
			d.Overrides = p.parseEnvOverrides()
		default:
			p.errorf(fpos, "unknown environment field %q (want overrides)", field)
			p.syncLine()
		}
	}
	p.expect(KindRBrace, "to close environment body")
	return d
}

// parseEnvOverrides parses `overrides { agents { … } policies { … } }`.
func (p *parser) parseEnvOverrides() *EnvOverridesBlock {
	b := &EnvOverridesBlock{Pos: p.cur.Pos}
	if _, ok := p.expect(KindLBrace, "to open overrides block"); !ok {
		return b
	}
	seen := map[string]bool{}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected an overrides field (agents, policies), got %s", p.cur)
			p.syncLine()
			continue
		}
		field, fpos := p.cur.Lit, p.cur.Pos
		p.advance()
		if seen[field] {
			p.errorf(fpos, "duplicate overrides field %q", field)
		}
		seen[field] = true
		switch field {
		case "agents":
			b.Agents = p.parseAgentOverrides()
		case "policies":
			b.Policies = p.parsePolicyOverrides()
		default:
			p.errorf(fpos, "unknown overrides field %q (want agents or policies)", field)
			p.syncLine()
		}
	}
	p.expect(KindRBrace, "to close overrides block")
	return b
}

// parseAgentOverrides parses `agents { <name> { model … constraints { … } } … }`.
func (p *parser) parseAgentOverrides() []*AgentOverrideEntry {
	if _, ok := p.expect(KindLBrace, "to open agents override block"); !ok {
		return nil
	}
	var out []*AgentOverrideEntry
	seen := map[string]bool{}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected an agent name in agents { … }, got %s", p.cur)
			p.syncLine()
			continue
		}
		e := &AgentOverrideEntry{Pos: p.cur.Pos, Name: p.ident("for the agent override name")}
		name := identName(e.Name)
		if seen[name] {
			p.errorf(e.Pos, "duplicate agent override %q", name)
		}
		seen[name] = true
		if _, ok := p.expect(KindLBrace, "to open agent override body"); !ok {
			continue
		}
		fseen := map[string]bool{}
		for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
			if p.cur.Kind != KindIdent {
				p.errorf(p.cur.Pos, "expected an agent override field (model, constraints), got %s", p.cur)
				p.syncLine()
				continue
			}
			field, fpos := p.cur.Lit, p.cur.Pos
			p.advance()
			if fseen[field] {
				p.errorf(fpos, "duplicate agent override field %q", field)
			}
			fseen[field] = true
			switch field {
			case "model":
				e.Model = p.parseModelRef()
			case "constraints":
				e.Constraints = p.parseConstraints()
			default:
				p.errorf(fpos, "unknown agent override field %q (want model or constraints)", field)
				p.syncLine()
			}
		}
		p.expect(KindRBrace, "to close agent override body")
		out = append(out, e)
	}
	p.expect(KindRBrace, "to close agents override block")
	return out
}

// parsePolicyOverrides parses `policies { <name> { execution { … } approvals { … } } … }`.
func (p *parser) parsePolicyOverrides() []*PolicyOverrideEntry {
	if _, ok := p.expect(KindLBrace, "to open policies override block"); !ok {
		return nil
	}
	var out []*PolicyOverrideEntry
	seen := map[string]bool{}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected a policy name in policies { … }, got %s", p.cur)
			p.syncLine()
			continue
		}
		e := &PolicyOverrideEntry{Pos: p.cur.Pos, Name: p.ident("for the policy override name")}
		name := identName(e.Name)
		if seen[name] {
			p.errorf(e.Pos, "duplicate policy override %q", name)
		}
		seen[name] = true
		if _, ok := p.expect(KindLBrace, "to open policy override body"); !ok {
			continue
		}
		fseen := map[string]bool{}
		for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
			if p.cur.Kind != KindIdent {
				p.errorf(p.cur.Pos, "expected a policy override field (execution, approvals), got %s", p.cur)
				p.syncLine()
				continue
			}
			field, fpos := p.cur.Lit, p.cur.Pos
			p.advance()
			if fseen[field] {
				p.errorf(fpos, "duplicate policy override field %q", field)
			}
			fseen[field] = true
			switch field {
			case "execution":
				e.Execution = p.parsePolicyExecution()
			case "approvals":
				e.Approvals = p.parsePolicyApprovals()
			default:
				p.errorf(fpos, "unknown policy override field %q (want execution or approvals)", field)
				p.syncLine()
			}
		}
		p.expect(KindRBrace, "to close policy override body")
		out = append(out, e)
	}
	p.expect(KindRBrace, "to close policies override block")
	return out
}
