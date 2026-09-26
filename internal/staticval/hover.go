// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package staticval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

// AttributeInfo describes a provider attribute for hover.
type AttributeInfo struct {
	Type        string
	Description string
	Required    bool
	Optional    bool
	Computed    bool
	Sensitive   bool
	Deprecated  bool
	// DocsURL links the documentation of the resource or data source.
	DocsURL string
}

// Env gives the hover access to things outside the module.
type Env struct {
	// Attribute returns the provider schema of an attribute of a resource
	// ("resource") or data source ("data") type, if a schema is known.
	Attribute func(blockType, typeName, attr string) (*AttributeInfo, bool)
	// ModuleDir resolves a module source to its directory on disk. Local
	// sources are resolved without it.
	ModuleDir func(source string) (string, bool)
	// LoadModule loads a module directory.
	LoadModule func(dir string) (*Module, error)
}

// Hover is the markdown answer for a position and the range it covers.
type Hover struct {
	Content string
	Range   hcl.Range
}

// maxListed caps the instances and inputs listed in one hover.
const maxListed = 8

// HoverAt answers a hover at pos in the module file filename (a base
// name) when the position is on something this package knows more about
// than the schema: a variable or local reference or declaration, an
// each/count reference, a module output reference, a module call, or a
// resource or data attribute reference. It returns false otherwise.
func (ev *Evaluator) HoverAt(filename string, pos hcl.Pos, env Env) (*Hover, bool) {
	if ev.host == nil {
		ev.SetEnv(env)
	}
	f, ok := ev.mod.Files[filename]
	if !ok {
		return nil, false
	}
	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		return nil, false
	}

	for _, block := range body.Blocks {
		if !block.Range().ContainsPos(pos) {
			continue
		}
		if len(block.LabelRanges) > 0 && block.LabelRanges[0].ContainsPos(pos) {
			rng := block.LabelRanges[0]
			switch block.Type {
			case "variable":
				return &Hover{Content: ev.variableHover(block.Labels[0], nil), Range: rng}, true
			case "module":
				return &Hover{Content: ev.moduleCallHover(block, env), Range: rng}, true
			case "output":
				if content, ok := ev.outputHover(block.Labels[0]); ok {
					return &Hover{Content: content, Range: rng}, true
				}
			}
			return nil, false
		}
		if block.Type == "locals" {
			for name, attr := range block.Body.Attributes {
				if attr.NameRange.ContainsPos(pos) {
					return &Hover{Content: ev.localHover(name, nil), Range: attr.NameRange}, true
				}
			}
		}
		if block.Type == "module" {
			if attr, ok := block.Body.Attributes["source"]; ok && attr.Expr.Range().ContainsPos(pos) {
				return &Hover{Content: ev.moduleCallHover(block, env), Range: attr.Expr.Range()}, true
			}
		}
	}

	expr := traversalAt(body, pos)
	if expr == nil {
		return nil, false
	}
	t := expr.Traversal
	rng := expr.Range()
	switch t.RootName() {
	case "var":
		name, ok := attrStep(t, 1)
		if !ok {
			return nil, false
		}
		if _, ok := ev.vars[name]; !ok {
			return nil, false
		}
		return &Hover{Content: ev.variableHover(name, expr), Range: rng}, true
	case "local":
		name, ok := attrStep(t, 1)
		if !ok {
			return nil, false
		}
		if _, ok := ev.locals[name]; !ok {
			return nil, false
		}
		return &Hover{Content: ev.localHover(name, expr), Range: rng}, true
	case "each", "count":
		content, ok := ev.iterationHover(body, expr)
		if !ok {
			return nil, false
		}
		return &Hover{Content: content, Range: rng}, true
	case "module":
		content, ok := ev.moduleOutputHover(expr, env)
		if !ok {
			return nil, false
		}
		return &Hover{Content: content, Range: rng}, true
	case "terraform":
		if name, ok := attrStep(t, 1); ok && name == "workspace" {
			return &Hover{Content: fmt.Sprintf("`terraform.workspace` _string_\n\n**Value** `%s`\n\nThe selected workspace (from `.terraform/environment`, else `default`). `TF_WORKSPACE` can select another one.", quoteString(ev.mod.Workspace)), Range: rng}, true
		}
		return nil, false
	case "path", "self":
		return nil, false
	case "data":
		if len(t) < 4 {
			return nil, false
		}
		content, ok := ev.attributeHover("data", t, 1, env)
		if !ok {
			return nil, false
		}
		return &Hover{Content: content, Range: rng}, true
	}
	if len(t) < 3 {
		return nil, false
	}
	content, ok := ev.attributeHover("resource", t, 0, env)
	if !ok {
		return nil, false
	}
	return &Hover{Content: content, Range: rng}, true
}

// WantsHover reports, from the file alone, whether HoverAt may answer at
// pos. It lets callers skip loading the module for every other position.
func WantsHover(f *hcl.File, pos hcl.Pos) bool {
	if f == nil {
		return false
	}
	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		return false
	}
	for _, block := range body.Blocks {
		if !block.Range().ContainsPos(pos) {
			continue
		}
		if len(block.LabelRanges) > 0 && block.LabelRanges[0].ContainsPos(pos) {
			return block.Type == "variable" || block.Type == "module" || block.Type == "output"
		}
		switch block.Type {
		case "locals":
			for _, attr := range block.Body.Attributes {
				if attr.NameRange.ContainsPos(pos) {
					return true
				}
			}
		case "module":
			if attr, ok := block.Body.Attributes["source"]; ok && attr.Expr.Range().ContainsPos(pos) {
				return true
			}
		}
	}
	expr := traversalAt(body, pos)
	if expr == nil || len(expr.Traversal) < 2 {
		return false
	}
	switch expr.Traversal.RootName() {
	case "path", "self":
		return false
	case "terraform":
		name, _ := attrStep(expr.Traversal, 1)
		return name == "workspace"
	}
	return true
}

// traversalAt returns the innermost scope traversal containing pos.
func traversalAt(body *hclsyntax.Body, pos hcl.Pos) *hclsyntax.ScopeTraversalExpr {
	var found *hclsyntax.ScopeTraversalExpr
	hclsyntax.VisitAll(body, func(n hclsyntax.Node) hcl.Diagnostics {
		if e, ok := n.(*hclsyntax.ScopeTraversalExpr); ok && e.Range().ContainsPos(pos) {
			found = e
		}
		return nil
	})
	return found
}

func (ev *Evaluator) variableHover(name string, ref *hclsyntax.ScopeTraversalExpr) string {
	v := ev.vars[name]
	var b strings.Builder

	typeInline, typeBlock := typeDisplay(v)
	fmt.Fprintf(&b, "`var.%s`", name)
	if typeInline != "" {
		fmt.Fprintf(&b, " _%s_", typeInline)
	}
	b.WriteString("\n\n")
	if typeBlock != "" {
		fmt.Fprintf(&b, "```hcl\ntype = %s\n```\n\n", typeBlock)
	}
	if v.Description != "" {
		b.WriteString(v.Description)
		b.WriteString("\n\n")
	}

	// The value of a deeper reference such as var.network.zones.
	if ref != nil && len(ref.Traversal) > 2 {
		r := ev.Eval(ref, nil)
		b.WriteString(valueLine(TraversalString(ref.Traversal), r))
		b.WriteString("\n\n")
	}

	val, source, ok := v.Effective()
	switch {
	case ev.mod.RootPath != "":
		b.WriteString(ev.callValuesText(v))
	case !ok && len(v.Assignments) > 0 && v.Assignments[len(v.Assignments)-1].Err != "":
		a := v.Assignments[len(v.Assignments)-1]
		fmt.Fprintf(&b, "**Value** invalid: the value in `%s` does not match the type (%s)\n\n", a.File, a.Err)
	case !ok:
		fmt.Fprintf(&b, "**Value** unknown: no default and no tfvars value. Set it with `-var`, `-var-file` or `TF_VAR_%s`.\n\n", name)
	default:
		from := "the default"
		if source != "default" {
			from = "`" + source + "`"
		}
		if v.Sensitive {
			fmt.Fprintf(&b, "**Value** %s from %s\n\n", sensitiveText, from)
		} else {
			b.WriteString(valueBlock("**Value**", val, "from "+from))
		}
		if overridden := ev.overriddenSources(v, source); overridden != "" {
			b.WriteString(overridden)
			b.WriteString("\n\n")
		}
		switch {
		case source == "default":
			fmt.Fprintf(&b, "_May be overridden by `-var`, `-var-file` or `TF_VAR_%s`._\n\n", name)
		default:
			b.WriteString("_May be overridden by `-var` or `-var-file`._\n\n")
		}
	}

	for i, a := range v.Assignments {
		if a.Err != "" && i < len(v.Assignments)-1 {
			fmt.Fprintf(&b, "Warning: the value in `%s` does not match the type (%s)\n\n", a.File, a.Err)
		}
	}

	var flags []string
	if v.Sensitive {
		flags = append(flags, "sensitive")
	}
	if v.Ephemeral {
		flags = append(flags, "ephemeral")
	}
	if !v.Nullable {
		flags = append(flags, "not nullable")
	}
	switch v.Validations {
	case 0:
	case 1:
		flags = append(flags, "1 validation rule")
	default:
		flags = append(flags, fmt.Sprintf("%d validation rules", v.Validations))
	}
	if v.Deprecated != "" {
		flags = append(flags, "deprecated: "+v.Deprecated)
	}
	if len(flags) > 0 {
		b.WriteString(strings.Join(flags, " · "))
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "_Declared in `%s`_", v.File)
	return b.String()
}

// callValuesText lists the value each module call passes to a child
// module variable.
func (ev *Evaluator) callValuesText(v *Variable) string {
	var b strings.Builder
	if len(v.CallValues) == 0 {
		b.WriteString("**Value** set by the module call")
		if v.HasDefault && !v.Sensitive {
			fmt.Fprintf(&b, " (default `%s`)", FormatCompact(v.Default, 40))
		}
		b.WriteString("\n\n")
		return b.String()
	}
	if val, ok := v.callValue(); ok {
		if v.Sensitive || val.ContainsMarked() {
			fmt.Fprintf(&b, "**Value** %s", sensitiveText)
		} else {
			b.WriteString(strings.TrimSuffix(valueBlock("**Value**", val, ""), "\n\n"))
		}
		if len(v.CallValues) > 1 {
			fmt.Fprintf(&b, " in all %d module calls", len(v.CallValues))
		}
		b.WriteString("\n\n")
	} else {
		b.WriteString("**Value** by module call:\n\n")
	}
	for _, cv := range v.CallValues {
		fmt.Fprintf(&b, "- `%s` (`%s`): ", cv.Call, cv.File)
		switch {
		case !cv.Set && cv.Result.IsKnown():
			if v.Sensitive {
				b.WriteString("default")
			} else {
				fmt.Fprintf(&b, "default `%s`", FormatCompact(cv.Result.Value, 50))
			}
		case cv.Result.IsKnown() && (v.Sensitive || cv.Result.IsSensitive()):
			b.WriteString(sensitiveText)
		case cv.Result.IsKnown():
			fmt.Fprintf(&b, "`%s`", FormatCompact(cv.Result.Value, 50))
		default:
			b.WriteString(unknownExplanation(cv.Result))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return b.String()
}

func (ev *Evaluator) overriddenSources(v *Variable, winner string) string {
	var parts []string
	for i := len(v.Assignments) - 1; i >= 0; i-- {
		a := v.Assignments[i]
		if a.File == winner {
			continue
		}
		switch {
		case a.Value.IsNull() && !v.Nullable && winner == "default":
			parts = append(parts, fmt.Sprintf("`%s` sets `null`, which the default replaces because the variable is not nullable", a.File))
		case v.Sensitive:
			parts = append(parts, fmt.Sprintf("`%s`", a.File))
		default:
			parts = append(parts, fmt.Sprintf("`%s` sets `%s`", a.File, FormatCompact(a.Value, 40)))
		}
	}
	if v.HasDefault && winner != "default" {
		if v.Sensitive {
			parts = append(parts, "the default")
		} else {
			parts = append(parts, fmt.Sprintf("default `%s`", FormatCompact(v.Default, 40)))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "Overrides: " + strings.Join(parts, "; ")
}

// typeDisplay returns the type constraint either for the header line or,
// when it spans several lines or is long, for a code block.
func typeDisplay(v *Variable) (string, string) {
	src := strings.TrimSpace(v.TypeSource)
	if src == "" {
		if v.Type == cty.DynamicPseudoType {
			return "any", ""
		}
		return v.Type.FriendlyName(), ""
	}
	if !strings.Contains(src, "\n") && utf8.RuneCountInString(src) <= 40 {
		return src, ""
	}
	return "", dedent(src)
}

func (ev *Evaluator) localHover(name string, ref *hclsyntax.ScopeTraversalExpr) string {
	l := ev.locals[name]
	r := ev.EvalLocal(name)
	var b strings.Builder

	fmt.Fprintf(&b, "`local.%s`", name)
	if r.IsKnown() && !r.IsSensitive() {
		fmt.Fprintf(&b, " _%s_", r.Value.Type().FriendlyNameForConstraint())
	}
	b.WriteString("\n\n")

	if ref != nil && len(ref.Traversal) > 2 {
		b.WriteString(valueLine(TraversalString(ref.Traversal), ev.Eval(ref, nil)))
		b.WriteString("\n\n")
	}
	if r.Kind == PerCall && len(ev.mod.Callers) > 0 {
		b.WriteString("**Value** by module call:\n\n")
		for _, pc := range ev.perCall(func(child *Evaluator) Result { return child.EvalLocal(name) }) {
			fmt.Fprintf(&b, "- `%s`: %s\n", pc.call, compactResult(pc.result, 50))
		}
		b.WriteString("\n")
	} else {
		b.WriteString(resultBlock("**Value**", r))
	}
	fmt.Fprintf(&b, "```hcl\n%s = %s\n```\n\n", name, trimSource(l.Source))
	fmt.Fprintf(&b, "_Defined in `%s`_", l.File)
	return b.String()
}

// callResult is a result for one module call of a child module.
type callResult struct {
	call   string
	result Result
}

// perCall evaluates fn once per known module call of this child module,
// with the variables that call passes.
func (ev *Evaluator) perCall(fn func(child *Evaluator) Result) []callResult {
	var out []callResult
	parents := make(map[*Module]*Evaluator)
	for _, c := range ev.mod.Callers {
		parentEv, ok := parents[c.Parent]
		if !ok {
			parentEv = NewEvaluator(c.Parent)
			if ev.host != nil {
				parentEv.SetEnv(*ev.host)
			}
			parents[c.Parent] = parentEv
		}
		block := parentEv.findModuleCall(c.Name)
		if block == nil {
			continue
		}
		mod := *ev.mod
		mod.Callers = nil
		mc := &moduleCall{block: block, dir: ev.mod.Path, child: &mod}
		out = append(out, callResult{call: "module." + c.Name, result: fn(parentEv.childEvaluator(mc))})
	}
	return out
}

// compactResult renders a result on one line.
func compactResult(r Result, max int) string {
	switch {
	case r.IsKnown() && r.IsSensitive():
		return sensitiveText
	case r.IsKnown():
		return "`" + FormatCompact(r.Value, max) + "`"
	}
	return unknownExplanation(r)
}

// resultBlock renders an evaluation result with its label.
func resultBlock(label string, r Result) string {
	switch {
	case r.IsKnown() && r.IsSensitive():
		return label + " " + sensitiveText + "\n\n"
	case r.IsKnown():
		return valueBlock(label, r.Value, "")
	}
	return label + " " + unknownExplanation(r) + "\n\n"
}

// unknownExplanation says why a value is not known, never guessing it.
func unknownExplanation(r Result) string {
	switch r.Kind {
	case AfterApply:
		if r.Detail != "" {
			return fmt.Sprintf("known after apply: depends on `%s` (%s)", r.Reason, r.Detail)
		}
		return fmt.Sprintf("known after apply: depends on `%s`", r.Reason)
	case NoInput:
		return fmt.Sprintf("unknown: `%s` has no default or tfvars value", r.Reason)
	case AtPlan:
		if strings.HasPrefix(r.Reason, "data.") {
			return fmt.Sprintf("known during the plan, which reads the data source: depends on `%s`", r.Reason)
		}
		return fmt.Sprintf("known during the plan: depends on `%s`", r.Reason)
	case PerCall:
		return fmt.Sprintf("set by the module calls: depends on `%s`", r.Reason)
	case NotEvaluated:
		if strings.HasSuffix(r.Reason, "()") {
			return fmt.Sprintf("not evaluated: `%s` is not evaluated statically", r.Reason)
		}
		return "not evaluated: " + r.Reason
	}
	return "unknown"
}

// valueLine renders "`ref` = value" for a sub-reference, on one line.
func valueLine(ref string, r Result) string {
	switch {
	case r.IsKnown() && r.IsSensitive():
		return fmt.Sprintf("`%s` = %s", ref, sensitiveText)
	case r.IsKnown():
		s := FormatValue(r.Value)
		if strings.Contains(s, "\n") {
			return fmt.Sprintf("`%s` =\n```hcl\n%s\n```", ref, capValueText(s))
		}
		return fmt.Sprintf("`%s` = `%s`", ref, truncate(s, maxValueChars))
	}
	return fmt.Sprintf("`%s`: %s", ref, unknownExplanation(r))
}

// valueBlock renders a label and a value inline when it fits on one line,
// or as an HCL code block.
func valueBlock(label string, v cty.Value, suffix string) string {
	s := FormatValue(v)
	if suffix != "" {
		suffix = " " + suffix
	}
	if pretty, ok := prettyJSON(v); ok {
		return fmt.Sprintf("%s (a JSON string)%s\n```json\n%s\n```\n\n", label, suffix, capValueText(pretty))
	}
	if strings.Contains(s, "\n") {
		return fmt.Sprintf("%s%s\n```hcl\n%s\n```\n\n", label, suffix, capValueText(s))
	}
	return fmt.Sprintf("%s `%s`%s\n\n", label, truncate(s, maxValueChars), suffix)
}

// maxValueLines and maxValueChars cap a value shown in a hover, which
// otherwise can be megabytes (a file() of a large JSON document).
const (
	maxValueLines = 60
	maxValueChars = 6000
)

// capValueText keeps the first lines of a rendered value and says how
// many are left out.
func capValueText(s string) string {
	lines := strings.Split(s, "\n")
	cut := 0
	if len(lines) > maxValueLines {
		cut = len(lines) - maxValueLines
		lines = lines[:maxValueLines]
	}
	out := strings.Join(lines, "\n")
	if utf8.RuneCountInString(out) > maxValueChars {
		out = string([]rune(out)[:maxValueChars])
		if i := strings.LastIndex(out, "\n"); i > 0 {
			out = out[:i]
		}
		cut = len(strings.Split(s, "\n")) - len(strings.Split(out, "\n"))
	}
	if cut > 0 {
		out += fmt.Sprintf("\n… %d more %s", cut, plural(cut, "line", "lines"))
	}
	return out
}

// prettyJSON indents a string value that holds a JSON object or array,
// such as the result of jsonencode, which reads badly when escaped.
func prettyJSON(v cty.Value) (string, bool) {
	if v.IsMarked() || !v.IsKnown() || v.IsNull() || v.Type() != cty.String {
		return "", false
	}
	s := strings.TrimSpace(v.AsString())
	if len(s) < 2 || (s[0] != '{' && s[0] != '[') || !json.Valid([]byte(s)) {
		return "", false
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(s), "", "  "); err != nil {
		return "", false
	}
	return buf.String(), true
}

const maxSourceLines = 12
const maxSourceChars = 800

// trimSource keeps an expression's source short enough for a hover.
func trimSource(src string) string {
	src = dedent(strings.TrimSpace(src))
	lines := strings.Split(src, "\n")
	cut := false
	if len(lines) > maxSourceLines {
		lines = lines[:maxSourceLines]
		cut = true
	}
	out := strings.Join(lines, "\n")
	if utf8.RuneCountInString(out) > maxSourceChars {
		out = string([]rune(out)[:maxSourceChars])
		cut = true
	}
	if cut {
		out += "\n# …"
	}
	return out
}

// dedent removes the common indentation of every line but the first,
// which starts where the expression starts.
func dedent(src string) string {
	lines := strings.Split(src, "\n")
	if len(lines) < 2 {
		return src
	}
	common := -1
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		n := len(line) - len(strings.TrimLeft(line, " \t"))
		if common < 0 || n < common {
			common = n
		}
	}
	// The closing line of a block usually has the base indentation.
	if common <= 0 {
		return src
	}
	for i := 1; i < len(lines); i++ {
		if len(lines[i]) >= common {
			lines[i] = lines[i][common:]
		} else {
			lines[i] = strings.TrimLeft(lines[i], " \t")
		}
	}
	return strings.Join(lines, "\n")
}

// enclosingResource returns the resource, data or module block of the
// top-level body that contains pos.
func enclosingResource(body *hclsyntax.Body, pos hcl.Pos) *hclsyntax.Block {
	for _, block := range body.Blocks {
		if !block.Range().ContainsPos(pos) {
			continue
		}
		switch block.Type {
		case "resource", "data", "module", "ephemeral":
			return block
		}
	}
	return nil
}

func blockAddress(block *hclsyntax.Block) string {
	switch block.Type {
	case "data":
		return "data." + strings.Join(block.Labels, ".")
	case "module":
		return "module." + strings.Join(block.Labels, ".")
	case "ephemeral":
		return "ephemeral." + strings.Join(block.Labels, ".")
	}
	return strings.Join(block.Labels, ".")
}

// Instance is one instance of a resource with for_each or count.
type Instance struct {
	Key   cty.Value
	Value cty.Value
	// Each holds the values of each or count for this instance.
	Each map[string]cty.Value
}

// Instances evaluates the for_each or count of a block. It returns the
// meta-argument name, its result and the instances when known.
func (ev *Evaluator) Instances(block *hclsyntax.Block) (string, Result, []Instance, bool) {
	if attr, ok := block.Body.Attributes["for_each"]; ok {
		r := ev.Eval(attr.Expr, nil)
		if !r.IsKnown() || r.IsSensitive() || r.Value.IsNull() {
			return "for_each", r, nil, true
		}
		v := r.Value
		ty := v.Type()
		var insts []Instance
		switch {
		case ty.IsMapType() || ty.IsObjectType():
			for it := v.ElementIterator(); it.Next(); {
				k, e := it.Element()
				insts = append(insts, Instance{Key: k, Value: e})
			}
		case ty.IsSetType() && ty.ElementType() != cty.String:
			// OpenTofu rejects a set of numbers, for example, even
			// though it could convert them
			return "for_each", Result{Value: cty.DynamicVal, Kind: NotEvaluated, Reason: "for_each must be a map or a set of strings, not " + ty.FriendlyName()}, nil, true
		case ty.IsSetType():
			for it := v.ElementIterator(); it.Next(); {
				_, e := it.Element()
				insts = append(insts, Instance{Key: e, Value: e})
			}
		default:
			return "for_each", Result{Value: cty.DynamicVal, Kind: NotEvaluated, Reason: "for_each must be a map or a set of strings, not " + ty.FriendlyName()}, nil, true
		}
		for i := range insts {
			insts[i].Each = map[string]cty.Value{
				"each": cty.ObjectVal(map[string]cty.Value{"key": insts[i].Key, "value": insts[i].Value}),
			}
		}
		return "for_each", r, insts, true
	}
	if attr, ok := block.Body.Attributes["count"]; ok {
		r := ev.Eval(attr.Expr, nil)
		if !r.IsKnown() || r.IsSensitive() || r.Value.IsNull() {
			return "count", r, nil, true
		}
		n, err := convertInt(r.Value)
		if err != nil {
			return "count", Result{Value: cty.DynamicVal, Kind: NotEvaluated, Reason: err.Error()}, nil, true
		}
		insts := make([]Instance, n)
		for i := 0; i < n; i++ {
			idx := cty.NumberIntVal(int64(i))
			insts[i] = Instance{Key: idx, Value: idx, Each: map[string]cty.Value{
				"count": cty.ObjectVal(map[string]cty.Value{"index": idx}),
			}}
		}
		return "count", r, insts, true
	}
	return "", Result{}, nil, false
}

func convertInt(v cty.Value) (int, error) {
	if v.IsMarked() {
		v, _ = v.Unmark()
	}
	if v.Type() != cty.Number {
		// count converts, for example, the string "3"
		n, err := convert.Convert(v, cty.Number)
		if err != nil || n.IsNull() {
			return 0, fmt.Errorf("count must be a number")
		}
		v = n
	}
	bf := v.AsBigFloat()
	if !bf.IsInt() {
		return 0, fmt.Errorf("count must be a whole number")
	}
	i, _ := bf.Int64()
	if i < 0 {
		return 0, fmt.Errorf("count must not be negative")
	}
	if i > 10000 {
		return 0, fmt.Errorf("count is too large to list")
	}
	return int(i), nil
}

func (ev *Evaluator) iterationHover(body *hclsyntax.Body, expr *hclsyntax.ScopeTraversalExpr) (string, bool) {
	block := enclosingResource(body, expr.Range().Start)
	if block == nil {
		return "", false
	}
	meta, r, insts, ok := ev.Instances(block)
	if !ok {
		return "", false
	}
	root := expr.Traversal.RootName()
	if root == "each" && meta != "for_each" || root == "count" && meta != "count" {
		return "", false
	}

	ref := TraversalString(expr.Traversal)
	var b strings.Builder
	fmt.Fprintf(&b, "`%s` in `%s`\n\n", ref, blockAddress(block))
	switch ref {
	case "each.key":
		b.WriteString("The map key (or set member) of this instance.\n\n")
	case "each.value":
		b.WriteString("The map value of this instance (for a set, the same as `each.key`).\n\n")
	case "count.index":
		b.WriteString("The index of this instance, starting at 0.\n\n")
	}
	metaSrc := sourceText(ev.mod.Files[filepath.Base(block.Range().Filename)], block.Body.Attributes[meta].Expr.Range())
	fmt.Fprintf(&b, "`%s = %s`", meta, oneLineSource(metaSrc))
	if !r.IsKnown() && r.Kind == PerCall && len(ev.mod.Callers) > 0 {
		b.WriteString(" differs between module calls:\n\n")
		for _, pc := range ev.perCall(func(child *Evaluator) Result {
			_, cr, insts, _ := child.Instances(block)
			if !cr.IsKnown() {
				return cr
			}
			keys := make([]cty.Value, 0, len(insts))
			for _, inst := range insts {
				keys = append(keys, inst.Key)
			}
			if len(keys) == 0 {
				return Result{Value: cty.EmptyTupleVal, Kind: Known}
			}
			return Result{Value: cty.TupleVal(keys), Kind: Known}
		}) {
			if pc.result.IsKnown() && !pc.result.IsSensitive() {
				n := pc.result.Value.LengthInt()
				fmt.Fprintf(&b, "- `%s`: %d %s `%s`\n", pc.call, n, plural(n, "instance", "instances"), FormatCompact(pc.result.Value, 50))
				continue
			}
			fmt.Fprintf(&b, "- `%s`: %s\n", pc.call, compactResult(pc.result, 50))
		}
		return b.String(), true
	}
	if !r.IsKnown() {
		fmt.Fprintf(&b, ": %s\n\n", unknownExplanation(r))
		if r.Kind == AfterApply {
			fmt.Fprintf(&b, "OpenTofu cannot plan a `%s` that is only known after apply.\n\n", meta)
		}
		return b.String(), true
	}
	if r.IsSensitive() {
		b.WriteString(" is sensitive\n\n")
		return b.String(), true
	}
	switch len(insts) {
	case 1:
		b.WriteString(": 1 instance\n\n")
	default:
		fmt.Fprintf(&b, ": %d instances\n\n", len(insts))
	}

	for i, inst := range insts {
		if i == maxListed {
			fmt.Fprintf(&b, "- and %d more\n", len(insts)-maxListed)
			break
		}
		vr := ev.Eval(expr, inst.Each)
		val := "`" + FormatCompact(vr.Value, 60) + "`"
		switch {
		case vr.IsKnown() && vr.IsSensitive():
			val = sensitiveText
		case !vr.IsKnown():
			val = unknownExplanation(vr)
		}
		if meta == "count" || ref == "each.key" || r.Value.Type().IsSetType() {
			fmt.Fprintf(&b, "- %s\n", val)
			continue
		}
		fmt.Fprintf(&b, "- `%s`: %s\n", FormatCompact(inst.Key, 40), val)
	}
	return b.String(), true
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func oneLineSource(src string) string {
	fields := strings.Fields(src)
	s := strings.Join(fields, " ")
	return truncate(s, 80)
}

func (ev *Evaluator) attributeHover(blockType string, t hcl.Traversal, typeStep int, env Env) (string, bool) {
	if env.Attribute == nil {
		return "", false
	}
	var typeName string
	if typeStep == 0 {
		typeName = t.RootName()
	} else {
		name, ok := attrStep(t, typeStep)
		if !ok {
			return "", false
		}
		typeName = name
	}
	// Skip the name and an optional instance key.
	i := typeStep + 2
	if i < len(t) {
		if _, ok := t[i].(hcl.TraverseIndex); ok {
			i++
		}
	}
	if i >= len(t) {
		return "", false
	}
	attrName, ok := t[i].(hcl.TraverseAttr)
	if !ok {
		return "", false
	}
	info, ok := env.Attribute(blockType, typeName, attrName.Name)
	if !ok {
		return "", false
	}

	var b strings.Builder
	fmt.Fprintf(&b, "`%s`", TraversalString(t))
	if info.Type != "" {
		fmt.Fprintf(&b, " _%s_", info.Type)
	}
	b.WriteString("\n\n")

	var flags []string
	switch {
	case info.Required:
		flags = append(flags, "required")
	case info.Optional && info.Computed:
		flags = append(flags, "optional", "computed")
	case info.Optional:
		flags = append(flags, "optional")
	case info.Computed && blockType == "data":
		flags = append(flags, ev.dataComputedFlag(typeName, t, typeStep))
	case info.Computed:
		flags = append(flags, "computed: **known after apply**")
	}
	if info.Sensitive {
		flags = append(flags, "**sensitive**")
	}
	if info.Deprecated {
		flags = append(flags, "**deprecated**")
	}
	if len(flags) > 0 {
		b.WriteString(strings.Join(flags, " · "))
		b.WriteString("\n\n")
	}
	if !(info.Computed && !info.Optional && !info.Required) {
		b.WriteString(ev.configuredValue(blockType, typeName, t, typeStep, attrName.Name, info))
	}
	if info.Description != "" {
		b.WriteString(info.Description)
		b.WriteString("\n\n")
	}
	if info.DocsURL != "" {
		fmt.Fprintf(&b, "[`%s` documentation](%s)", typeName, info.DocsURL)
	}
	return strings.TrimSpace(b.String()), true
}

// dataComputedFlag says when a computed attribute of a data source is
// known: during the plan, or after apply when the data source is read
// during apply.
func (ev *Evaluator) dataComputedFlag(typeName string, t hcl.Traversal, typeStep int) string {
	name, ok := attrStep(t, typeStep+1)
	if !ok {
		return "computed"
	}
	r := ev.DataReadTiming(typeName, name)
	switch r.Kind {
	case AtPlan:
		return "computed: read during the plan"
	case AfterApply:
		return "computed: **known after apply** (" + r.Detail + ")"
	}
	return "computed: read during the plan if its arguments are known then (" + unknownExplanation(r) + ")"
}

// configuredValue shows the value an attribute gets from the resource's
// own configuration.
func (ev *Evaluator) configuredValue(blockType, typeName string, t hcl.Traversal, typeStep int, attr string, info *AttributeInfo) string {
	name, ok := attrStep(t, typeStep+1)
	if !ok {
		return ""
	}
	block := ev.findBlock(blockType, typeName, name)
	if block == nil {
		return ""
	}
	a, ok := block.Body.Attributes[attr]
	if !ok {
		if info.Optional && info.Computed {
			return "**Value** not set in the configuration, so **known after apply**\n\n"
		}
		if info.Optional {
			return "**Value** not set in the configuration (`null`)\n\n"
		}
		return ""
	}
	src := trimSource(sourceText(ev.mod.Files[filepath.Base(block.Range().Filename)], a.Expr.Range()))
	if block.Body.Attributes["for_each"] != nil || block.Body.Attributes["count"] != nil {
		return fmt.Sprintf("Configured per instance:\n```hcl\n%s = %s\n```\n\n", attr, src)
	}
	r := ev.Eval(a.Expr, nil)
	if info.Sensitive && r.IsKnown() {
		r.Value = r.Value.Mark(SensitiveMark)
	}
	return resultBlock("**Value**", r) + fmt.Sprintf("```hcl\n%s = %s\n```\n\n", attr, src)
}

// findBlock returns the resource or data block with the given labels.
func (ev *Evaluator) findBlock(blockType, typeName, name string) *hclsyntax.Block {
	for _, fname := range ev.mod.sortedFileNames() {
		body, ok := ev.mod.Files[fname].Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		for _, block := range body.Blocks {
			if block.Type == blockType && len(block.Labels) == 2 && block.Labels[0] == typeName && block.Labels[1] == name {
				return block
			}
		}
	}
	return nil
}

// moduleCall is a module block of the module with its resolved child.
type moduleCall struct {
	block  *hclsyntax.Block
	source string
	dir    string
	child  *Module
}

func (ev *Evaluator) findModuleCall(name string) *hclsyntax.Block {
	for _, fname := range ev.mod.sortedFileNames() {
		body, ok := ev.mod.Files[fname].Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		for _, block := range body.Blocks {
			if block.Type == "module" && len(block.Labels) == 1 && block.Labels[0] == name {
				return block
			}
		}
	}
	return nil
}

func (ev *Evaluator) resolveModuleCall(block *hclsyntax.Block, env Env) (*moduleCall, bool) {
	mc := &moduleCall{block: block}
	attr, ok := block.Body.Attributes["source"]
	if !ok {
		return mc, false
	}
	r := ev.Eval(attr.Expr, nil)
	if !r.IsKnown() || r.Value.Type() != cty.String || r.Value.IsMarked() {
		return mc, false
	}
	mc.source = r.Value.AsString()
	if strings.HasPrefix(mc.source, "./") || strings.HasPrefix(mc.source, "../") {
		mc.dir = filepath.Join(ev.mod.Path, mc.source)
	} else if env.ModuleDir != nil {
		if dir, ok := env.ModuleDir(mc.source); ok {
			mc.dir = dir
		}
	}
	if mc.dir == "" || env.LoadModule == nil {
		return mc, false
	}
	child, err := env.LoadModule(mc.dir)
	if err != nil {
		return mc, false
	}
	child.RootPath = ev.mod.Path
	if ev.mod.RootPath != "" {
		child.RootPath = ev.mod.RootPath
	}
	mc.child = child
	return mc, true
}

// childEvaluator returns an evaluator for the called module whose
// variables take the values this module call passes.
func (ev *Evaluator) childEvaluator(mc *moduleCall) *Evaluator {
	return ev.childEvaluatorFor(mc, nil)
}

// childEvaluatorFor is childEvaluator for one instance of a call with
// for_each or count, whose each or count values are given.
func (ev *Evaluator) childEvaluatorFor(mc *moduleCall, each map[string]cty.Value) *Evaluator {
	child := NewEvaluator(mc.child)
	child.depth = ev.depth + 1
	if ev.host != nil {
		// Nested module calls evaluate their outputs too, up to
		// maxModuleDepth levels.
		child.SetEnv(*ev.host)
	}
	for name, v := range child.vars {
		cv := ev.callValueForInstance(mc.block, v, mc.child.Path, each)
		v.CallValues = []CallValue{cv}
		delete(child.varCauses, name)
		val := cv.Result.Value
		if !cv.Result.IsKnown() {
			val = cty.DynamicVal
			child.varCauses[name] = cv.Result
		}
		if v.Sensitive {
			val = val.Mark(SensitiveMark)
		}
		child.varValues[name] = val
	}
	return child
}

type outputDecl struct {
	name        string
	file        string
	description string
	sensitive   bool
	expr        hcl.Expression
	source      string
}

func (m *Module) outputs() map[string]*outputDecl {
	outs := make(map[string]*outputDecl)
	for _, fname := range m.sortedFileNames() {
		f := m.Files[fname]
		content, _, _ := f.Body.PartialContent(&hcl.BodySchema{
			Blocks: []hcl.BlockHeaderSchema{{Type: "output", LabelNames: []string{"name"}}},
		})
		if content == nil {
			continue
		}
		for _, block := range content.Blocks {
			attrs, _ := block.Body.JustAttributes()
			o := &outputDecl{name: block.Labels[0], file: fname}
			if prev, ok := outs[o.name]; ok && IsOverrideFile(fname) {
				// An override output merges into the primary one.
				o = prev
			}
			if a, ok := attrs["description"]; ok {
				if v, diags := a.Expr.Value(nil); !diags.HasErrors() && v.Type() == cty.String && !v.IsNull() {
					o.description = v.AsString()
				}
			}
			if a, ok := attrs["sensitive"]; ok {
				if v, diags := a.Expr.Value(nil); !diags.HasErrors() && v.Type() == cty.Bool && !v.IsNull() {
					o.sensitive = o.sensitive || v.True()
				}
			}
			if a, ok := attrs["value"]; ok {
				o.expr = a.Expr
				o.source = sourceText(f, a.Expr.Range())
			}
			outs[o.name] = o
		}
	}
	return outs
}

func (ev *Evaluator) moduleOutputHover(expr *hclsyntax.ScopeTraversalExpr, env Env) (string, bool) {
	t := expr.Traversal
	callName, ok := attrStep(t, 1)
	if !ok {
		return "", false
	}
	block := ev.findModuleCall(callName)
	if block == nil {
		return "", false
	}
	// module.<call>[<key>].<output> refers to one instance
	outStep := 2
	var instanceKey cty.Value
	if len(t) > 2 {
		if idx, ok := t[2].(hcl.TraverseIndex); ok {
			instanceKey = idx.Key
			outStep = 3
		}
	}
	outName, hasOut := attrStep(t, outStep)
	if !hasOut {
		if outStep == 3 {
			return "", false
		}
		return ev.moduleCallHover(block, env), true
	}
	mc, ok := ev.resolveModuleCall(block, env)
	if !ok {
		return "", false
	}
	out, ok := mc.child.outputs()[outName]
	if !ok {
		return "", false
	}

	var b strings.Builder
	fmt.Fprintf(&b, "`%s` · output of `%s`\n\n", TraversalString(t), mc.source)
	if out.description != "" {
		b.WriteString(out.description)
		b.WriteString("\n\n")
	}
	if out.expr != nil {
		_, hasForEach := block.Body.Attributes["for_each"]
		_, hasCount := block.Body.Attributes["count"]
		var r Result
		switch {
		case (hasForEach || hasCount) && instanceKey != cty.NilVal:
			r = ev.instanceOutput(block, mc, out, instanceKey)
		case hasForEach:
			r = Result{Kind: NotEvaluated, Reason: "the module call has for_each"}
		case hasCount:
			r = Result{Kind: NotEvaluated, Reason: "the module call has count"}
		default:
			r = ev.childEvaluator(mc).Eval(out.expr, nil)
		}
		if out.sensitive && r.IsKnown() {
			r.Value = r.Value.Mark(SensitiveMark)
		}
		if len(t) > outStep+1 && r.IsKnown() {
			// A deeper reference, such as module.x.out.attr.
			if sub, diags := t[outStep+1:].TraverseRel(r.Value); !diags.HasErrors() {
				r.Value = sub
			}
		}
		b.WriteString(resultBlock("**Value**", r))
		fmt.Fprintf(&b, "```hcl\nvalue = %s\n```\n\n", trimSource(out.source))
	}
	if out.sensitive {
		b.WriteString("sensitive\n\n")
	}
	fmt.Fprintf(&b, "_Defined in `%s`_", relPath(ev.mod.Path, filepath.Join(mc.dir, out.file)))
	return b.String(), true
}

// instanceOutput evaluates an output for the instance with the given key
// of a module call with for_each or count.
func (ev *Evaluator) instanceOutput(block *hclsyntax.Block, mc *moduleCall, out *outputDecl, key cty.Value) Result {
	_, r, insts, _ := ev.Instances(block)
	if !r.IsKnown() {
		return r
	}
	if r.IsSensitive() {
		return Result{Value: cty.DynamicVal, Kind: NotEvaluated, Reason: "the instances of the module call are sensitive"}
	}
	for _, inst := range insts {
		if eq := inst.Key.Equals(key); eq.IsKnown() && eq.True() {
			return ev.childEvaluatorFor(mc, inst.Each).Eval(out.expr, nil)
		}
	}
	return Result{Value: cty.DynamicVal, Kind: NotEvaluated, Reason: "the module call has no instance " + FormatCompact(key, 40)}
}

// outputHover shows an output of this module with its value.
func (ev *Evaluator) outputHover(name string) (string, bool) {
	out, ok := ev.mod.outputs()[name]
	if !ok {
		return "", false
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**output** `%s`\n\n", name)
	if out.description != "" {
		b.WriteString(out.description)
		b.WriteString("\n\n")
	}
	if out.expr != nil {
		r := ev.Eval(out.expr, nil)
		if out.sensitive && r.IsKnown() {
			r.Value = r.Value.Mark(SensitiveMark)
		}
		if r.Kind == PerCall && len(ev.mod.Callers) > 0 {
			b.WriteString("**Value** by module call:\n\n")
			for _, pc := range ev.perCall(func(child *Evaluator) Result {
				cr := child.Eval(out.expr, nil)
				if out.sensitive && cr.IsKnown() {
					cr.Value = cr.Value.Mark(SensitiveMark)
				}
				return cr
			}) {
				fmt.Fprintf(&b, "- `%s`: %s\n", pc.call, compactResult(pc.result, 50))
			}
			b.WriteString("\n")
		} else {
			b.WriteString(resultBlock("**Value**", r))
		}
		fmt.Fprintf(&b, "```hcl\nvalue = %s\n```\n\n", trimSource(out.source))
	}
	if out.sensitive {
		b.WriteString("sensitive\n\n")
	}
	return strings.TrimSpace(b.String()), true
}

func relPath(base, target string) string {
	if rel, err := filepath.Rel(base, target); err == nil {
		return filepath.ToSlash(rel)
	}
	return target
}

func (ev *Evaluator) moduleCallHover(block *hclsyntax.Block, env Env) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**module** `%s`", block.Labels[0])
	mc, ok := ev.resolveModuleCall(block, env)
	if mc.source != "" {
		fmt.Fprintf(&b, " · source `%s`", mc.source)
	}
	b.WriteString("\n\n")
	if !ok {
		if mc.dir == "" && mc.source != "" {
			b.WriteString("The module is not installed; run `tofu init` to see its inputs and outputs.")
		}
		return strings.TrimSpace(b.String())
	}
	fmt.Fprintf(&b, "Directory `%s`\n\n", relPath(ev.mod.Path, mc.dir))

	child := NewEvaluator(mc.child)
	names := mapKeys(child.vars)
	sort.SliceStable(names, func(i, j int) bool {
		ri := !child.vars[names[i]].HasDefault
		rj := !child.vars[names[j]].HasDefault
		return ri && !rj
	})
	if len(names) > 0 {
		b.WriteString("**Inputs**\n\n")
	}
	for _, name := range names {
		v := child.vars[name]
		typ, _ := typeDisplay(v)
		if typ == "" {
			typ = v.Type.FriendlyNameForConstraint()
		}
		fmt.Fprintf(&b, "- `%s` _%s_", name, typ)
		_, set := block.Body.Attributes[name]
		switch {
		case !v.HasDefault && !set:
			b.WriteString(" · **required, not set**")
		case !v.HasDefault:
			b.WriteString(" · required")
		case set:
			b.WriteString(" · optional")
		case v.Sensitive:
			b.WriteString(" · default " + sensitiveText)
		default:
			fmt.Fprintf(&b, " · default `%s`", FormatCompact(v.Default, 30))
		}
		if set {
			cv := ev.callValueFor(block, v, mc.child.Path)
			switch {
			case cv.Result.IsKnown() && (v.Sensitive || cv.Result.IsSensitive()):
				b.WriteString(" = " + sensitiveText)
			case cv.Result.IsKnown():
				fmt.Fprintf(&b, " = `%s`", FormatCompact(cv.Result.Value, 40))
			}
		}
		if v.Description != "" {
			fmt.Fprintf(&b, ": %s", firstSentence(v.Description))
		}
		b.WriteString("\n")
	}
	outs := mc.child.outputs()
	if len(outs) > 0 {
		b.WriteString("\n**Outputs**\n\n")
	}
	for _, name := range mapKeys(outs) {
		o := outs[name]
		fmt.Fprintf(&b, "- `%s`", name)
		if o.sensitive {
			b.WriteString(" · sensitive")
		}
		if o.description != "" {
			fmt.Fprintf(&b, ": %s", firstSentence(o.description))
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 120)
}
