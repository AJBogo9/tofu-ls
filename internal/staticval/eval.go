// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package staticval

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/opentofu/opentofu-schema/earlydecoder"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	"github.com/zclconf/go-cty/cty/function"
)

// SensitiveMark marks values that come from a sensitive variable. The
// cty functions and HCL operators carry it into every derived value.
const SensitiveMark = earlydecoder.SensitiveMark

// Kind says whether a value is known and, if not, why.
type Kind int

const (
	// Known means the value is wholly known without a plan.
	Known Kind = iota
	// AfterApply means the value depends on a resource or data attribute,
	// a module output or an impure function.
	AfterApply
	// NoInput means the value depends on a variable that has no default
	// and no tfvars value.
	NoInput
	// NotEvaluated means the expression could not be evaluated statically,
	// for example because it calls an unsupported function or is invalid.
	NotEvaluated
	// AtPlan means the value depends on a data source, which is read
	// during the plan.
	AtPlan
	// PerCall means the value depends on a variable of a child module
	// that its module calls set to different values, or that callers not
	// resolved set.
	PerCall
)

// Result is the outcome of evaluating an expression.
type Result struct {
	Value cty.Value
	Kind  Kind
	// Reason names what made the value unknown: a reference such as
	// "random_pet.name.id" or "var.project", a function call such as
	// "timestamp()", or an error message.
	Reason string
}

// IsKnown reports whether the whole value is known.
func (r Result) IsKnown() bool {
	return r.Kind == Known
}

// IsSensitive reports whether any part of the value is sensitive.
func (r Result) IsSensitive() bool {
	return r.Value != cty.NilVal && r.Value.ContainsMarked()
}

// Variable is a variable declaration with every value OpenTofu would
// consider for it.
type Variable struct {
	Name        string
	File        string
	DeclRange   hcl.Range
	Description string

	Type         cty.Type
	TypeDefaults *typeexpr.Defaults
	// TypeSource is the type constraint as written, or "" without one.
	TypeSource string

	HasDefault    bool
	Default       cty.Value
	DefaultSource string

	Sensitive   bool
	Nullable    bool
	Ephemeral   bool
	Deprecated  string
	Validations int

	// Assignments are the tfvars values in the order OpenTofu applies
	// them; the last one wins.
	Assignments []Assignment

	// CallValues are the values the module calls pass, for a child module.
	CallValues []CallValue
}

// CallValue is the value one module call passes to a child's variable.
type CallValue struct {
	// Call is the module call address, such as "module.app".
	Call string
	// File is the file of the module call, relative to the child.
	File string
	// Set is false when the call leaves the variable to its default.
	Set    bool
	Result Result
}

// Assignment is a value given to a variable in a tfvars file.
type Assignment struct {
	File  string
	Range hcl.Range
	Value cty.Value
	// Err is set when the value does not convert to the variable's type.
	Err string
}

// Local is one entry of a locals block.
type Local struct {
	Name      string
	File      string
	NameRange hcl.Range
	Expr      hcl.Expression
	Source    string
}

// Evaluator evaluates expressions in the context of one module. It is
// not safe for concurrent use.
type Evaluator struct {
	mod   *Module
	funcs map[string]function.Function

	vars      map[string]*Variable
	locals    map[string]*Local
	varValues map[string]cty.Value
	// varCauses says why a variable's value is unknown when a module call
	// passes an unknown value.
	varCauses map[string]Result

	localResults map[string]Result
	evaluating   map[string]bool

	host *Env
	// moduleOutputs caches the evaluated outputs of each module call.
	moduleOutputs map[string]map[string]Result
}

// SetEnv lets the evaluator load child modules, so that references to
// module outputs evaluate to the output's value for that call.
func (ev *Evaluator) SetEnv(host Env) {
	ev.host = &host
}

// NewEvaluator decodes the variables and locals of mod.
func NewEvaluator(mod *Module) *Evaluator {
	ev := &Evaluator{
		mod:          mod,
		funcs:        earlydecoder.StaticFunctions(mod.Path),
		vars:         make(map[string]*Variable),
		locals:       make(map[string]*Local),
		varValues:    make(map[string]cty.Value),
		varCauses:    make(map[string]Result),
		localResults: make(map[string]Result),
		evaluating:   make(map[string]bool),

		moduleOutputs: make(map[string]map[string]Result),
	}
	ev.decode()
	return ev
}

// Module returns the module the evaluator works on.
func (ev *Evaluator) Module() *Module {
	return ev.mod
}

// Variable returns the declaration of a variable.
func (ev *Evaluator) Variable(name string) (*Variable, bool) {
	v, ok := ev.vars[name]
	return v, ok
}

// Local returns the declaration of a local.
func (ev *Evaluator) Local(name string) (*Local, bool) {
	l, ok := ev.locals[name]
	return l, ok
}

var rootSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "variable", LabelNames: []string{"name"}},
		{Type: "locals"},
	},
}

var variableSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "description"},
		{Name: "type"},
		{Name: "default"},
		{Name: "sensitive"},
		{Name: "nullable"},
		{Name: "ephemeral"},
		{Name: "deprecated"},
	},
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "validation"},
	},
}

func (ev *Evaluator) decode() {
	for _, name := range ev.mod.sortedFileNames() {
		f := ev.mod.Files[name]
		content, _, _ := f.Body.PartialContent(rootSchema)
		if content == nil {
			continue
		}
		for _, block := range content.Blocks {
			switch block.Type {
			case "variable":
				ev.decodeVariable(name, f, block)
			case "locals":
				attrs, _ := block.Body.JustAttributes()
				for attrName, attr := range attrs {
					ev.locals[attrName] = &Local{
						Name:      attrName,
						File:      name,
						NameRange: attr.NameRange,
						Expr:      attr.Expr,
						Source:    sourceText(f, attr.Expr.Range()),
					}
				}
			}
		}
	}

	varsFiles := VarsFileOrder(mapKeys(ev.mod.VarsFiles))
	if ev.mod.RootPath != "" {
		// OpenTofu reads tfvars files only for the root module.
		varsFiles = nil
	}
	for _, name := range varsFiles {
		f := ev.mod.VarsFiles[name]
		attrs, _ := f.Body.JustAttributes()
		for attrName, attr := range attrs {
			v, ok := ev.vars[attrName]
			if !ok {
				continue
			}
			val, diags := attr.Expr.Value(nil)
			if diags.HasErrors() {
				continue
			}
			a := Assignment{File: name, Range: attr.Range, Value: val}
			if _, err := v.convert(val); err != nil {
				a.Err = err.Error()
			}
			v.Assignments = append(v.Assignments, a)
		}
	}

	for _, c := range ev.mod.Callers {
		parentEv := NewEvaluator(c.Parent)
		block := parentEv.findModuleCall(c.Name)
		if block == nil {
			continue
		}
		for _, v := range ev.vars {
			v.CallValues = append(v.CallValues, parentEv.callValueFor(block, v, ev.mod.Path))
		}
	}

	for name, v := range ev.vars {
		val, _, ok := v.Effective()
		if !ok {
			val = cty.DynamicVal
		}
		if ev.mod.RootPath != "" {
			// A child module gets its values from the module calls.
			val = cty.DynamicVal
			ev.varCauses[name] = Result{Kind: PerCall, Reason: "var." + name}
			if cv, ok := v.callValue(); ok {
				val = cv
				delete(ev.varCauses, name)
			} else if r, ok := v.callCause(); ok {
				ev.varCauses[name] = r
			}
		}
		if v.Sensitive {
			val = val.Mark(SensitiveMark)
		}
		ev.varValues[name] = val
	}
}

// callValueFor evaluates the value that the module call block of this
// (parent) module passes to the child variable v.
func (ev *Evaluator) callValueFor(block *hclsyntax.Block, v *Variable, childPath string) CallValue {
	cv := CallValue{
		Call: "module." + block.Labels[0],
		File: relPath(childPath, block.Range().Filename),
	}
	attr, set := block.Body.Attributes[v.Name]
	cv.Set = set
	switch {
	case set && (block.Body.Attributes["for_each"] != nil || block.Body.Attributes["count"] != nil):
		cv.Result = Result{Value: cty.DynamicVal, Kind: NotEvaluated, Reason: cv.Call + " has several instances (for_each or count)"}
	case set:
		cv.Result = ev.Eval(attr.Expr, nil)
		if cv.Result.IsKnown() {
			val, err := v.convert(cv.Result.Value)
			if err != nil {
				cv.Result = Result{Value: cty.DynamicVal, Kind: NotEvaluated, Reason: fmt.Sprintf("%s passes a value that does not match the type: %s", cv.Call, err)}
			} else {
				cv.Result.Value = val
			}
		}
	case v.HasDefault:
		val, err := v.convert(v.Default)
		if err != nil {
			cv.Result = Result{Value: cty.DynamicVal, Kind: NotEvaluated, Reason: err.Error()}
		} else {
			cv.Result = Result{Value: val, Kind: Known}
		}
	default:
		cv.Result = Result{Value: cty.DynamicVal, Kind: NotEvaluated, Reason: cv.Call + " does not set the required variable " + v.Name}
	}
	return cv
}

// callValue returns the value of a child module variable when every
// module call gives it the same known value.
func (v *Variable) callValue() (cty.Value, bool) {
	if len(v.CallValues) == 0 {
		return cty.NilVal, false
	}
	var val cty.Value
	for i, cv := range v.CallValues {
		if !cv.Result.IsKnown() {
			return cty.NilVal, false
		}
		if i == 0 {
			val = cv.Result.Value
			continue
		}
		eq, _ := val.Equals(cv.Result.Value).Unmark()
		if !eq.IsKnown() || eq.IsNull() || eq.False() {
			return cty.NilVal, false
		}
	}
	return val, true
}

// callCause explains an unknown child module variable: the first unknown
// call value, or PerCall when the calls disagree.
func (v *Variable) callCause() (Result, bool) {
	if len(v.CallValues) == 0 {
		return Result{}, false
	}
	for _, cv := range v.CallValues {
		if !cv.Result.IsKnown() {
			return cv.Result, true
		}
	}
	return Result{Kind: PerCall, Reason: "var." + v.Name}, true
}

func (ev *Evaluator) decodeVariable(filename string, f *hcl.File, block *hcl.Block) {
	name := block.Labels[0]
	v := &Variable{
		Name:      name,
		File:      filename,
		DeclRange: block.DefRange,
		Type:      cty.DynamicPseudoType,
		Nullable:  true,
	}
	ev.vars[name] = v

	content, _, _ := block.Body.PartialContent(variableSchema)
	if content == nil {
		return
	}
	for _, b := range content.Blocks {
		if b.Type == "validation" {
			v.Validations++
		}
	}
	if attr, ok := content.Attributes["description"]; ok {
		if val, diags := attr.Expr.Value(nil); !diags.HasErrors() && val.Type() == cty.String && val.IsKnown() && !val.IsNull() {
			v.Description = val.AsString()
		}
	}
	if attr, ok := content.Attributes["type"]; ok {
		ty, defaults, diags := typeexpr.TypeConstraintWithDefaults(attr.Expr)
		if !diags.HasErrors() {
			v.Type = ty
			v.TypeDefaults = defaults
		}
		v.TypeSource = sourceText(f, attr.Expr.Range())
	}
	for _, flag := range []struct {
		name string
		dst  *bool
	}{
		{"sensitive", &v.Sensitive},
		{"nullable", &v.Nullable},
		{"ephemeral", &v.Ephemeral},
	} {
		if attr, ok := content.Attributes[flag.name]; ok {
			if val, diags := attr.Expr.Value(nil); !diags.HasErrors() && val.Type() == cty.Bool && val.IsKnown() && !val.IsNull() {
				*flag.dst = val.True()
			}
		}
	}
	if attr, ok := content.Attributes["deprecated"]; ok {
		if val, diags := attr.Expr.Value(nil); !diags.HasErrors() && val.Type() == cty.String && val.IsKnown() && !val.IsNull() {
			v.Deprecated = val.AsString()
		}
	}
	if attr, ok := content.Attributes["default"]; ok {
		if val, diags := attr.Expr.Value(nil); !diags.HasErrors() {
			v.HasDefault = true
			v.Default = val
			v.DefaultSource = sourceText(f, attr.Expr.Range())
		}
	}
}

// convert applies optional attribute defaults and converts val to the
// variable's type, as OpenTofu does for every input value.
func (v *Variable) convert(val cty.Value) (cty.Value, error) {
	if v.TypeDefaults != nil && !val.IsNull() {
		val = v.TypeDefaults.Apply(val)
	}
	return convert.Convert(val, v.Type)
}

// Effective returns the value OpenTofu would use when no -var, -var-file
// or TF_VAR_ environment variable is given, and where it comes from: a
// tfvars file name, or "default".
func (v *Variable) Effective() (cty.Value, string, bool) {
	for i := len(v.Assignments) - 1; i >= 0; i-- {
		a := v.Assignments[i]
		val, err := v.convert(a.Value)
		if err != nil {
			return cty.NilVal, "", false
		}
		if val.IsNull() && !v.Nullable {
			break
		}
		return val, a.File, true
	}
	if v.HasDefault {
		val, err := v.convert(v.Default)
		if err != nil {
			return cty.NilVal, "", false
		}
		return val, "default", true
	}
	return cty.NilVal, "", false
}

// EvalLocal evaluates a local by name, following references to other
// locals recursively. A reference cycle evaluates to NotEvaluated.
func (ev *Evaluator) EvalLocal(name string) Result {
	if r, ok := ev.localResults[name]; ok {
		return r
	}
	l, ok := ev.locals[name]
	if !ok {
		return Result{Value: cty.DynamicVal, Kind: NotEvaluated, Reason: fmt.Sprintf("local.%s is not declared", name)}
	}
	if ev.evaluating[name] {
		return Result{Value: cty.DynamicVal, Kind: NotEvaluated, Reason: fmt.Sprintf("local.%s refers to itself", name)}
	}
	ev.evaluating[name] = true
	r := ev.Eval(l.Expr, nil)
	delete(ev.evaluating, name)
	ev.localResults[name] = r
	return r
}

// Eval evaluates expr in the module's scope. extra adds values for
// iteration symbols such as each and count.
func (ev *Evaluator) Eval(expr hcl.Expression, extra map[string]cty.Value) Result {
	if name, ok := unsupportedFunction(expr, ev.funcs); ok {
		return Result{Value: cty.DynamicVal, Kind: NotEvaluated, Reason: name + "()"}
	}

	ctx := ev.evalContext(expr.Variables(), extra)
	val, diags := expr.Value(ctx)
	if diags.HasErrors() {
		// An unknown operand can surface as an error in some operations,
		// so report an unknown dependency first when there is one.
		if r, ok := ev.unknownCause(expr, ctx); ok {
			r.Value = cty.DynamicVal
			return r
		}
		return Result{Value: cty.DynamicVal, Kind: NotEvaluated, Reason: diagSummary(diags)}
	}
	if val.IsWhollyKnown() {
		return Result{Value: val, Kind: Known}
	}
	if r, ok := ev.unknownCause(expr, ctx); ok {
		r.Value = val
		return r
	}
	return Result{Value: val, Kind: AfterApply, Reason: "an unknown value"}
}

func (ev *Evaluator) evalContext(traversals []hcl.Traversal, extra map[string]cty.Value) *hcl.EvalContext {
	vars := map[string]cty.Value{
		"var":       objectOrEmpty(ev.varValues),
		"path":      ev.pathValue(),
		"terraform": cty.ObjectVal(map[string]cty.Value{"workspace": cty.StringVal(ev.mod.Workspace)}),
	}
	locals := make(map[string]cty.Value)
	for _, t := range traversals {
		root := t.RootName()
		switch root {
		case "var", "path", "terraform":
			continue
		case "local":
			if name, ok := attrStep(t, 1); ok {
				if _, seen := locals[name]; !seen {
					locals[name] = ev.EvalLocal(name).Value
				}
			}
			continue
		}
		if _, ok := extra[root]; ok {
			continue
		}
		if root == "module" {
			continue
		}
		// Resources, data sources, module outputs, self and any other
		// symbol are only known during the plan.
		vars[root] = cty.DynamicVal
	}
	vars["local"] = objectOrEmpty(locals)
	vars["module"] = ev.moduleValue(traversals)
	for k, v := range extra {
		vars[k] = v
	}
	return &hcl.EvalContext{
		Variables: vars,
		Functions: ev.funcs,
	}
}

// moduleValue builds the module object for the module calls that the
// traversals refer to. Calls that cannot be evaluated are unknown.
func (ev *Evaluator) moduleValue(traversals []hcl.Traversal) cty.Value {
	calls := make(map[string]cty.Value)
	for _, t := range traversals {
		if t.RootName() != "module" {
			continue
		}
		name, ok := attrStep(t, 1)
		if !ok {
			continue
		}
		if _, seen := calls[name]; seen {
			continue
		}
		outs, ok := ev.evalModuleOutputs(name)
		if !ok {
			calls[name] = cty.DynamicVal
			continue
		}
		vals := make(map[string]cty.Value, len(outs))
		for k, r := range outs {
			vals[k] = r.Value
			if !r.IsKnown() {
				vals[k] = cty.DynamicVal
			}
		}
		calls[name] = objectOrEmpty(vals)
	}
	if len(calls) == 0 {
		return cty.DynamicVal
	}
	return cty.ObjectVal(calls)
}

// evalModuleOutputs evaluates every output of a module call without
// for_each or count, with the inputs this call passes.
func (ev *Evaluator) evalModuleOutputs(name string) (map[string]Result, bool) {
	if outs, ok := ev.moduleOutputs[name]; ok {
		return outs, outs != nil
	}
	ev.moduleOutputs[name] = nil
	if ev.host == nil {
		return nil, false
	}
	block := ev.findModuleCall(name)
	if block == nil || block.Body.Attributes["for_each"] != nil || block.Body.Attributes["count"] != nil {
		return nil, false
	}
	mc, ok := ev.resolveModuleCall(block, *ev.host)
	if !ok {
		return nil, false
	}
	child := ev.childEvaluator(mc)
	outs := make(map[string]Result)
	for outName, o := range mc.child.outputs() {
		if o.expr == nil {
			continue
		}
		r := child.Eval(o.expr, nil)
		if o.sensitive && r.IsKnown() {
			r.Value = r.Value.Mark(SensitiveMark)
		}
		outs[outName] = r
	}
	ev.moduleOutputs[name] = outs
	return outs, true
}

func (ev *Evaluator) pathValue() cty.Value {
	module := cty.StringVal(".")
	cwd := ev.mod.Path
	if ev.mod.RootPath != "" {
		cwd = ev.mod.RootPath
		if rel, err := filepath.Rel(ev.mod.RootPath, ev.mod.Path); err == nil {
			module = cty.StringVal(filepath.ToSlash(rel))
		}
	}
	return cty.ObjectVal(map[string]cty.Value{
		"module": module,
		"root":   cty.StringVal("."),
		"cwd":    cty.StringVal(filepath.ToSlash(cwd)),
	})
}

// unknownCause finds the first reference or call, in source order, whose
// value is unknown.
func (ev *Evaluator) unknownCause(expr hcl.Expression, ctx *hcl.EvalContext) (Result, bool) {
	traversals := expr.Variables()
	sort.SliceStable(traversals, func(i, j int) bool {
		return traversals[i].SourceRange().Start.Byte < traversals[j].SourceRange().Start.Byte
	})
	for _, t := range traversals {
		val, diags := t.TraverseAbs(ctx)
		if !diags.HasErrors() && val.IsWhollyKnown() {
			continue
		}
		switch t.RootName() {
		case "var":
			if name, ok := attrStep(t, 1); ok {
				if _, declared := ev.vars[name]; declared && !ev.varValues[name].IsWhollyKnown() {
					if cause, ok := ev.varCauses[name]; ok {
						return cause, true
					}
					return Result{Kind: NoInput, Reason: "var." + name}, true
				}
			}
			continue
		case "local":
			if name, ok := attrStep(t, 1); ok {
				r := ev.EvalLocal(name)
				if !r.IsKnown() {
					return r, true
				}
			}
			continue
		case "module":
			call, _ := attrStep(t, 1)
			out, _ := attrStep(t, 2)
			if r, ok := ev.moduleOutputs[call][out]; ok && !r.IsKnown() {
				return Result{Kind: r.Kind, Reason: TraversalString(t)}, true
			}
			if diags.HasErrors() {
				continue
			}
			return Result{Kind: AfterApply, Reason: TraversalString(t)}, true
		case "each", "count":
			if _, ok := ctx.Variables[t.RootName()]; !ok || diags.HasErrors() {
				continue
			}
			return Result{Kind: NotEvaluated, Reason: TraversalString(t) + " differs per instance"}, true
		}
		if diags.HasErrors() {
			continue
		}
		if t.RootName() == "data" {
			return Result{Kind: AtPlan, Reason: TraversalString(t)}, true
		}
		return Result{Kind: AfterApply, Reason: TraversalString(t)}, true
	}

	var fn string
	hclsyntax.VisitAll(syntaxNode(expr), func(n hclsyntax.Node) hcl.Diagnostics {
		if call, ok := n.(*hclsyntax.FunctionCallExpr); ok && fn == "" {
			if earlydecoder.ImpureFunctions[call.Name] {
				fn = call.Name
			}
		}
		return nil
	})
	if fn != "" {
		return Result{Kind: AfterApply, Reason: fn + "()"}, true
	}
	return Result{}, false
}

// unsupportedFunction returns the name of the first function called in
// expr that the static function table does not implement.
func unsupportedFunction(expr hcl.Expression, funcs map[string]function.Function) (string, bool) {
	node := syntaxNode(expr)
	if node == nil {
		return "", false
	}
	var name string
	hclsyntax.VisitAll(node, func(n hclsyntax.Node) hcl.Diagnostics {
		if call, ok := n.(*hclsyntax.FunctionCallExpr); ok && name == "" {
			if _, ok := funcs[call.Name]; !ok {
				name = call.Name
			}
		}
		return nil
	})
	return name, name != ""
}

func syntaxNode(expr hcl.Expression) hclsyntax.Node {
	if n, ok := expr.(hclsyntax.Node); ok {
		return n
	}
	return nil
}

// attrStep returns the attribute name at step i of a traversal.
func attrStep(t hcl.Traversal, i int) (string, bool) {
	if len(t) <= i {
		return "", false
	}
	switch s := t[i].(type) {
	case hcl.TraverseAttr:
		return s.Name, true
	case hcl.TraverseIndex:
		if s.Key.Type() == cty.String && s.Key.IsKnown() {
			return s.Key.AsString(), true
		}
	}
	return "", false
}

// TraversalString renders a traversal the way it is written, for example
// `random_pet.name.id` or `aws_instance.web[0].id`.
func TraversalString(t hcl.Traversal) string {
	var b strings.Builder
	for i, step := range t {
		switch s := step.(type) {
		case hcl.TraverseRoot:
			b.WriteString(s.Name)
		case hcl.TraverseAttr:
			b.WriteString(".")
			b.WriteString(s.Name)
		case hcl.TraverseIndex:
			b.WriteString("[")
			b.WriteString(FormatCompact(s.Key, 40))
			b.WriteString("]")
		case hcl.TraverseSplat:
			b.WriteString("[*]")
		default:
			if i > 0 {
				b.WriteString(".?")
			}
		}
	}
	return b.String()
}

func diagSummary(diags hcl.Diagnostics) string {
	for _, d := range diags {
		if d.Severity == hcl.DiagError {
			if d.Detail != "" {
				return d.Summary + ": " + d.Detail
			}
			return d.Summary
		}
	}
	return "invalid expression"
}

func sourceText(f *hcl.File, rng hcl.Range) string {
	if f == nil || rng.End.Byte > len(f.Bytes) || rng.Start.Byte > rng.End.Byte {
		return ""
	}
	return string(rng.SliceBytes(f.Bytes))
}

func objectOrEmpty(m map[string]cty.Value) cty.Value {
	if len(m) == 0 {
		return cty.EmptyObjectVal
	}
	return cty.ObjectVal(m)
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
