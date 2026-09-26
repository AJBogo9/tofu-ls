// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package staticval

import (
	"context"
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

// EphemeralMark marks values that come from an ephemeral variable or
// output. Like SensitiveMark it carries into every derived value, and
// OpenTofu never shows such a value ("(ephemeral value)").
const EphemeralMark = "ephemeral"

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
	// Rejected means OpenTofu refuses the value, so there is none: it
	// fails a validation rule, a tfvars file is invalid, or a meta-argument
	// or not nullable variable is null.
	Rejected
	// FromState means the value is an argument that lifecycle
	// ignore_changes keeps at its value in the state.
	FromState
)

// Result is the outcome of evaluating an expression.
type Result struct {
	Value cty.Value
	Kind  Kind
	// Reason names what made the value unknown: a reference such as
	// "random_pet.name.id" or "var.project", a function call such as
	// "timestamp()", or an error message.
	Reason string
	// Detail explains the reason further, for example why a data source
	// is read during apply.
	Detail string
}

// IsKnown reports whether the whole value is known.
func (r Result) IsKnown() bool {
	return r.Kind == Known
}

// IsSensitive reports whether any part of the value is sensitive (or
// ephemeral).
func (r Result) IsSensitive() bool {
	return r.Value != cty.NilVal && containsMarked(r.Value)
}

// containsMarked is cty's ContainsMarked without building the path of
// every element, which is most of its cost on a large value.
func containsMarked(v cty.Value) bool {
	if v.IsMarked() {
		return true
	}
	if !v.IsKnown() || v.IsNull() {
		return false
	}
	ty := v.Type()
	if !ty.IsCollectionType() && !ty.IsObjectType() && !ty.IsTupleType() {
		return false
	}
	for it := v.ElementIterator(); it.Next(); {
		if _, e := it.Element(); containsMarked(e) {
			return true
		}
	}
	return false
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
	// Rules are the validation blocks.
	Rules []ValidationRule

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
	Set bool
	// NullReplaced is true when the call passes null to a variable that is
	// not nullable, so that OpenTofu uses the default instead.
	NullReplaced bool
	Result       Result
}

// ValidationRule is a validation block of a variable.
type ValidationRule struct {
	// Range is the header of the validation block.
	Range        hcl.Range
	Condition    hcl.Expression
	ErrorMessage hcl.Expression
	// MessageSource is the error_message as written.
	MessageSource string
}

// ValidationFailure is a validation rule that a value fails.
type ValidationFailure struct {
	Range hcl.Range
	// Message is the rule's error_message, rendered when it can be.
	Message string
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
	ctx   context.Context
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
	// moduleInstances caches the outputs of each module call with
	// for_each (an object by instance key) or count (a tuple), or
	// cty.NilVal when they could not be evaluated.
	moduleInstances map[string]cty.Value
	// depth is how many module calls deep this evaluator is below the
	// evaluator that the hover started from.
	depth int
	// readingData guards against data sources that depend on each other.
	readingData map[string]bool
	// configured and dataTiming cache configuredAttribute and
	// DataReadTiming: chained resources and data sources would otherwise
	// evaluate each other exponentially often.
	configured map[string]cty.Value
	dataTiming map[string]Result
	// refused says why OpenTofu refuses to run this root module when an
	// automatically loaded tfvars file is invalid. It is the zero Result
	// otherwise.
	refused Result
	// failures holds the validation rules that each variable's value fails.
	failures map[string][]ValidationFailure
}

// maxModuleDepth caps how deep module outputs are evaluated through
// nested module calls.
const maxModuleDepth = 4

// SetEnv lets the evaluator load child modules, so that references to
// module outputs evaluate to the output's value for that call.
func (ev *Evaluator) SetEnv(host Env) {
	ev.host = &host
}

// NewEvaluator decodes the variables and locals of mod.
func NewEvaluator(mod *Module) *Evaluator {
	return NewEvaluatorContext(context.Background(), mod)
}

// NewEvaluatorContext is NewEvaluator for a request: once ctx is done,
// evaluations stop and return NotEvaluated.
func NewEvaluatorContext(ctx context.Context, mod *Module) *Evaluator {
	// OpenTofu resolves relative file paths against the working
	// directory, which is the root module, and path.module is relative to
	// it. Files may be read from the root's tree and the module's own.
	baseDir := mod.Path
	if mod.RootPath != "" {
		baseDir = mod.RootPath
	}
	ev := &Evaluator{
		ctx:          ctx,
		mod:          mod,
		funcs:        earlydecoder.StaticFunctions(baseDir, mod.Path),
		vars:         make(map[string]*Variable),
		locals:       make(map[string]*Local),
		varValues:    make(map[string]cty.Value),
		varCauses:    make(map[string]Result),
		localResults: make(map[string]Result),
		evaluating:   make(map[string]bool),
		readingData:  make(map[string]bool),
		configured:   make(map[string]cty.Value),
		dataTiming:   make(map[string]Result),
		failures:     make(map[string][]ValidationFailure),

		moduleOutputs:   make(map[string]map[string]Result),
		moduleInstances: make(map[string]cty.Value),
	}
	ev.decode()
	return ev
}

// newEvaluator is NewEvaluator for another module of the same request.
func (ev *Evaluator) newEvaluator(mod *Module) *Evaluator {
	return NewEvaluatorContext(ev.ctx, mod)
}

// cancelled reports whether the request that the evaluator serves is done.
func (ev *Evaluator) cancelled() bool {
	return ev.ctx != nil && ev.ctx.Err() != nil
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

	names := mapKeys(ev.mod.VarsFiles)
	for name := range ev.mod.VarsFileErrors {
		if _, ok := ev.mod.VarsFiles[name]; !ok {
			names = append(names, name)
		}
	}
	varsFiles := VarsFileOrder(names)
	if ev.mod.RootPath != "" {
		// OpenTofu reads tfvars files only for the root module.
		varsFiles = nil
	}
	for _, name := range varsFiles {
		if msg := ev.mod.VarsFileErrors[name]; msg != "" {
			ev.refuse(name, msg)
		}
		f := ev.mod.VarsFiles[name]
		if f == nil {
			continue
		}
		attrs, diags := f.Body.JustAttributes()
		if diags.HasErrors() {
			// for example a block, which tfvars files may not have
			ev.refuse(name, diagLine(diags))
		}
		for _, attrName := range mapKeys(attrs) {
			attr := attrs[attrName]
			// OpenTofu reads every value as a constant first, whether or
			// not a variable of that name is declared.
			val, diags := attr.Expr.Value(nil)
			if diags.HasErrors() {
				ev.refuse(name, diagLine(diags))
				continue
			}
			v, ok := ev.vars[attrName]
			if !ok {
				continue
			}
			a := Assignment{File: name, Range: attr.Range, Value: val}
			if _, err := v.convert(val); err != nil {
				a.Err = err.Error()
			}
			v.Assignments = append(v.Assignments, a)
		}
	}

	parents := make(map[*Module]*Evaluator)
	for _, c := range ev.mod.Callers {
		// several calls in one parent share its evaluator
		parentEv, ok := parents[c.Parent]
		if !ok {
			parentEv = ev.newEvaluator(c.Parent)
			parents[c.Parent] = parentEv
		}
		block := parentEv.findModuleCall(c.Name)
		if block == nil {
			continue
		}
		for _, v := range ev.vars {
			cv := parentEv.callValueFor(block, v, ev.mod.Path)
			ev.validateCallValue(v, &cv)
			v.CallValues = append(v.CallValues, cv)
		}
	}

	for name, v := range ev.vars {
		val, _, ok := v.Effective()
		if !ok {
			val = cty.DynamicVal
		}
		if ev.mod.RootPath == "" && ev.refused.Kind == Rejected {
			val = cty.DynamicVal
			ev.varCauses[name] = ev.refused
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
		ev.varValues[name] = v.markValue(val)
	}
	ev.applyValidations()
}

// refuse records that OpenTofu refuses to run the root module because the
// tfvars file name is invalid. The first invalid file is the one named.
func (ev *Evaluator) refuse(name, problem string) {
	if ev.refused.Kind == Rejected {
		return
	}
	ev.refused = Result{
		Value:  cty.DynamicVal,
		Kind:   Rejected,
		Reason: fmt.Sprintf("OpenTofu refuses to run: `%s` is invalid (%s)", name, problem),
	}
}

// diagLine renders the first error of diags with its line.
func diagLine(diags hcl.Diagnostics) string {
	for _, d := range diags {
		if d.Severity != hcl.DiagError {
			continue
		}
		msg := d.Summary
		if d.Detail != "" {
			msg += ": " + strings.TrimSuffix(d.Detail, ".")
		}
		if d.Subject != nil {
			return fmt.Sprintf("line %d: %s", d.Subject.Start.Line, msg)
		}
		return msg
	}
	return "invalid"
}

// markValue marks a value of v as sensitive or ephemeral, as declared.
func (v *Variable) markValue(val cty.Value) cty.Value {
	if v.Sensitive {
		val = val.Mark(SensitiveMark)
	}
	if v.Ephemeral {
		val = val.Mark(EphemeralMark)
	}
	return val
}

// redacted reports whether the values of v are never shown.
func (v *Variable) redacted() bool {
	return v.Sensitive || v.Ephemeral
}

// Validate evaluates the validation rules of the variable name for val,
// as OpenTofu checks every value a variable gets: its default, a tfvars
// value or a module call's argument. It returns the rules that val fails.
// A rule whose condition is not known statically is skipped, so every
// failure returned is certain, while no failures means only that none can
// be proven.
func (ev *Evaluator) Validate(name string, val cty.Value) []ValidationFailure {
	v, ok := ev.vars[name]
	if !ok || len(v.Rules) == 0 || val == cty.NilVal || !val.IsWhollyKnown() {
		return nil
	}
	// The rules see var.<name> as val. Nothing computed with it may leak
	// into the caches, nor anything cached with another value into the
	// rules, and Validate may run before SetEnv.
	savedVal, hadVal := ev.varValues[name]
	locals, configured, dataTiming := ev.localResults, ev.configured, ev.dataTiming
	outputs, instances := ev.moduleOutputs, ev.moduleInstances
	ev.varValues[name] = v.markValue(val)
	ev.localResults = make(map[string]Result)
	ev.configured = make(map[string]cty.Value)
	ev.dataTiming = make(map[string]Result)
	ev.moduleOutputs = make(map[string]map[string]Result)
	ev.moduleInstances = make(map[string]cty.Value)
	defer func() {
		if hadVal {
			ev.varValues[name] = savedVal
		} else {
			delete(ev.varValues, name)
		}
		ev.localResults, ev.configured, ev.dataTiming = locals, configured, dataTiming
		ev.moduleOutputs, ev.moduleInstances = outputs, instances
	}()

	var failures []ValidationFailure
	for _, rule := range v.Rules {
		if rule.Condition == nil {
			continue
		}
		r := ev.Eval(rule.Condition, nil)
		if !r.IsKnown() {
			continue
		}
		cond, _ := r.Value.UnmarkDeep()
		cond, err := convert.Convert(cond, cty.Bool)
		if err != nil || cond.IsNull() || cond.True() {
			continue
		}
		failures = append(failures, ValidationFailure{Range: rule.Range, Message: ev.errorMessage(rule)})
	}
	return failures
}

// errorMessage renders the error_message of a rule, or returns its source
// when it is not known statically. A message built from a sensitive or
// ephemeral value is not shown, as OpenTofu does not show it.
func (ev *Evaluator) errorMessage(rule ValidationRule) string {
	if rule.ErrorMessage == nil {
		return ""
	}
	r := ev.Eval(rule.ErrorMessage, nil)
	switch {
	case r.IsKnown() && r.IsSensitive() && redactedText(r.Value) == ephemeralText:
		return "(the error message includes an ephemeral value, so it is not shown)"
	case r.IsKnown() && r.IsSensitive():
		return "(the error message includes a sensitive value, so it is not shown)"
	case r.IsKnown() && r.Value.Type() == cty.String && !r.Value.IsNull():
		return strings.TrimSpace(r.Value.AsString())
	}
	return strings.TrimSpace(rule.MessageSource)
}

// validationResult is the Rejected result for a value that fails rules.
func validationResult(ref string, failures []ValidationFailure) Result {
	return Result{
		Value:  cty.DynamicVal,
		Kind:   Rejected,
		Reason: fmt.Sprintf("OpenTofu rejects the value of `%s`, which fails its validation: %q", ref, failures[0].Message),
	}
}

// validateCallValue checks the known value that a module call passes to v
// against v's rules. Only rules that need no other value of this module
// can be decided here, since the module's own values are not set yet.
func (ev *Evaluator) validateCallValue(v *Variable, cv *CallValue) {
	if !cv.Result.IsKnown() {
		return
	}
	if failures := ev.Validate(v.Name, cv.Result.Value); len(failures) > 0 {
		cv.Result = validationResult("var."+v.Name, failures)
	}
}

// applyValidations checks the value of every variable against its rules
// and makes the values that fail unknown, with the failure as the cause.
func (ev *Evaluator) applyValidations() {
	failed := make(map[string][]ValidationFailure)
	for _, name := range mapKeys(ev.vars) {
		if failures := ev.Validate(name, ev.varValues[name]); len(failures) > 0 {
			failed[name] = failures
		}
	}
	ev.failures = failed
	for name, failures := range failed {
		ev.varValues[name] = cty.DynamicVal
		ev.varCauses[name] = validationResult("var."+name, failures)
	}
}

// callValueFor evaluates the value that the module call block of this
// (parent) module passes to the child variable v.
func (ev *Evaluator) callValueFor(block *hclsyntax.Block, v *Variable, childPath string) CallValue {
	return ev.callValueForInstance(block, v, childPath, nil)
}

// callValueForInstance is callValueFor for one instance of a call with
// for_each or count, whose each or count values are given.
func (ev *Evaluator) callValueForInstance(block *hclsyntax.Block, v *Variable, childPath string, each map[string]cty.Value) CallValue {
	cv := CallValue{
		Call: "module." + block.Labels[0],
		File: relPath(childPath, block.Range().Filename),
	}
	attr, set := block.Body.Attributes[v.Name]
	cv.Set = set
	switch {
	case set && each == nil && (block.Body.Attributes["for_each"] != nil || block.Body.Attributes["count"] != nil):
		cv.Result = Result{Value: cty.DynamicVal, Kind: NotEvaluated, Reason: cv.Call + " differs per instance (for_each or count)"}
	case set:
		cv.Result = ev.Eval(attr.Expr, each)
		switch {
		case cv.Result.IsKnown() && cv.Result.Value.IsNull() && !v.Nullable && v.HasDefault:
			// OpenTofu uses the default for a null argument of a variable
			// that is not nullable.
			cv.NullReplaced = true
			_, marks := cv.Result.Value.Unmark()
			if val, err := v.convert(v.Default); err != nil {
				cv.Result = Result{Value: cty.DynamicVal, Kind: NotEvaluated, Reason: err.Error()}
			} else {
				cv.Result = Result{Value: val.WithMarks(marks), Kind: Known}
			}
		case cv.Result.IsKnown() && cv.Result.Value.IsNull() && !v.Nullable:
			cv.Result = Result{Value: cty.DynamicVal, Kind: Rejected, Reason: fmt.Sprintf("OpenTofu rejects %s: it sets the variable `%s`, which is not nullable and has no default, to null", cv.Call, v.Name)}
		case cv.Result.IsKnown():
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
	for _, cv := range v.CallValues {
		// Equal content is still sensitive or ephemeral when any call
		// marks it so.
		if cv.Result.IsSensitive() {
			_, marks := cv.Result.Value.UnmarkDeep()
			val = val.WithMarks(marks)
		}
	}
	return val, true
}

// callCause explains an unknown child module variable: the unknown value
// of its only module call, or PerCall when several calls disagree or some
// of them are unknown, so that the hover lists the value of each call.
func (v *Variable) callCause() (Result, bool) {
	if len(v.CallValues) == 0 {
		return Result{}, false
	}
	if len(v.CallValues) == 1 && !v.CallValues[0].Result.IsKnown() {
		return v.CallValues[0].Result, true
	}
	return Result{Kind: PerCall, Reason: "var." + v.Name}, true
}

func (ev *Evaluator) decodeVariable(filename string, f *hcl.File, block *hcl.Block) {
	name := block.Labels[0]
	v, merge := ev.vars[name]
	if !merge || !IsOverrideFile(filename) {
		// OpenTofu merges a declaration in an override file into the
		// primary one, attribute by attribute.
		merge = false
		v = &Variable{
			Name:      name,
			File:      filename,
			DeclRange: block.DefRange,
			Type:      cty.DynamicPseudoType,
			Nullable:  true,
		}
		ev.vars[name] = v
	}
	wasSensitive, wasEphemeral := v.Sensitive, v.Ephemeral
	defer func() {
		// An override never makes a sensitive or ephemeral value
		// displayable.
		v.Sensitive = v.Sensitive || wasSensitive
		v.Ephemeral = v.Ephemeral || wasEphemeral
	}()

	content, _, _ := block.Body.PartialContent(variableSchema)
	if content == nil {
		return
	}
	var rules []ValidationRule
	for _, b := range content.Blocks {
		if b.Type != "validation" {
			continue
		}
		rule := ValidationRule{Range: b.DefRange}
		attrs, _ := b.Body.JustAttributes()
		if a, ok := attrs["condition"]; ok {
			rule.Condition = a.Expr
		}
		if a, ok := attrs["error_message"]; ok {
			rule.ErrorMessage = a.Expr
			rule.MessageSource = sourceText(f, a.Expr.Range())
		}
		rules = append(rules, rule)
	}
	if !merge || len(rules) > 0 {
		v.Validations = len(rules)
		v.Rules = rules
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
	if ev.cancelled() {
		return Result{Value: cty.DynamicVal, Kind: NotEvaluated, Reason: "the request was cancelled"}
	}
	if name, ok := unsupportedFunction(expr, ev.funcs); ok {
		return Result{Value: cty.DynamicVal, Kind: NotEvaluated, Reason: name + "()"}
	}

	ctx := ev.evalContext(expr.Variables(), extra)
	if n := forIterations(expr, ctx); n > maxForIterations {
		// nested for expressions can build millions of values, which
		// would stall the hover and take hundreds of megabytes
		return Result{Value: cty.DynamicVal, Kind: NotEvaluated, Reason: fmt.Sprintf("its for expressions run about %d iterations, too many to evaluate statically", n)}
	}
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
	budget := maxValueNodes
	known, ok := wholeKnownWithin(val, &budget)
	if !ok {
		return Result{Value: cty.DynamicVal, Kind: NotEvaluated, Reason: fmt.Sprintf("the value has more than %d elements, too many to evaluate statically", maxValueNodes)}
	}
	if known {
		return Result{Value: val, Kind: Known}
	}
	if r, ok := ev.unknownCause(expr, ctx); ok {
		r.Value = val
		return r
	}
	// No reference or impure function explains the unknown part, so it
	// comes from a function that could not compute it statically, such as
	// a file function reading outside the module.
	reason := "part of the value cannot be computed statically"
	if callsFileFunction(expr) {
		reason = "reads a file outside the module, or one that cannot be read"
	}
	return Result{Value: val, Kind: NotEvaluated, Reason: reason}
}

// maxForIterations caps the iterations of (nested) for expressions that
// an evaluation may run.
const maxForIterations = 50000

// maxValueNodes caps the elements, at every depth, of a value that an
// evaluation inspects. A for expression that refers to a large list in
// each of its iterations builds a value with billions of elements that
// share memory, and walking it would take minutes.
const maxValueNodes = 2000000

// wholeKnownWithin is cty's IsWhollyKnown that gives up, with ok false,
// once it has visited budget elements. It stops at the first unknown one.
func wholeKnownWithin(v cty.Value, budget *int) (known, ok bool) {
	*budget--
	if *budget < 0 {
		return false, false
	}
	v, _ = v.Unmark()
	if !v.IsKnown() {
		return false, true
	}
	if v.IsNull() {
		return true, true
	}
	ty := v.Type()
	if !ty.IsCollectionType() && !ty.IsObjectType() && !ty.IsTupleType() {
		return true, true
	}
	for it := v.ElementIterator(); it.Next(); {
		_, e := it.Element()
		if known, ok := wholeKnownWithin(e, budget); !known || !ok {
			return known, ok
		}
	}
	return true, true
}

// forIterations estimates how many iterations the for expressions of expr
// run: for nested ones, the product of their collection lengths. A
// collection that cannot be evaluated up front (it depends on an outer
// iteration symbol) counts as one.
func forIterations(expr hcl.Expression, ctx *hcl.EvalContext) int {
	node := syntaxNode(expr)
	if node == nil {
		return 0
	}
	w := &forWalker{ctx: ctx, stack: []int{1}}
	hclsyntax.Walk(node, w)
	return w.max
}

type forWalker struct {
	ctx   *hcl.EvalContext
	stack []int
	max   int
}

func (w *forWalker) Enter(node hclsyntax.Node) hcl.Diagnostics {
	fe, ok := node.(*hclsyntax.ForExpr)
	if !ok {
		return nil
	}
	n := 1
	if coll, diags := fe.CollExpr.Value(w.ctx); !diags.HasErrors() && coll.IsKnown() && !coll.IsNull() {
		coll, _ = coll.Unmark()
		if ty := coll.Type(); ty.IsCollectionType() || ty.IsObjectType() || ty.IsTupleType() {
			n = coll.LengthInt()
		}
	}
	total := w.stack[len(w.stack)-1] * n
	if total > w.max {
		w.max = total
	}
	w.stack = append(w.stack, total)
	return nil
}

func (w *forWalker) Exit(node hclsyntax.Node) hcl.Diagnostics {
	if _, ok := node.(*hclsyntax.ForExpr); ok {
		w.stack = w.stack[:len(w.stack)-1]
	}
	return nil
}

// callsFileFunction reports whether expr calls a function that reads a
// file.
func callsFileFunction(expr hcl.Expression) bool {
	found := false
	hclsyntax.VisitAll(syntaxNode(expr), func(n hclsyntax.Node) hcl.Diagnostics {
		if call, ok := n.(*hclsyntax.FunctionCallExpr); ok {
			switch call.Name {
			case "file", "fileexists", "templatefile":
				found = true
			}
		}
		return nil
	})
	return found
}

func (ev *Evaluator) evalContext(traversals []hcl.Traversal, extra map[string]cty.Value) *hcl.EvalContext {
	vars := map[string]cty.Value{
		"var":       objectOrEmpty(ev.varValues),
		"path":      ev.pathValue(),
		"terraform": cty.ObjectVal(map[string]cty.Value{"workspace": cty.StringVal(ev.mod.Workspace)}),
	}
	locals := make(map[string]cty.Value)
	resources := make(map[string][]hcl.Traversal)
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
		if root != "data" && root != "self" && root != "ephemeral" {
			resources[root] = append(resources[root], t)
			continue
		}
		// Data sources, self and ephemeral resources are only known
		// during the plan or apply.
		vars[root] = cty.DynamicVal
	}
	for root, ts := range resources {
		vars[root] = ev.resourceValue(root, ts)
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

// resourceValue builds the value of the resources of one type that the
// traversals refer to. An argument the configuration sets is known
// during the plan, with the value it is set to, when the resource has no
// count or for_each and the provider schema says the attribute is not
// computed. Everything else is unknown.
func (ev *Evaluator) resourceValue(typeName string, traversals []hcl.Traversal) cty.Value {
	names := make(map[string]map[string]cty.Value)
	for _, t := range traversals {
		if len(t) < 3 {
			return cty.DynamicVal
		}
		name, ok1 := t[1].(hcl.TraverseAttr)
		attr, ok2 := t[2].(hcl.TraverseAttr)
		if !ok1 || !ok2 {
			// the whole object, or an instance of a counted resource
			return cty.DynamicVal
		}
		if names[name.Name] == nil {
			names[name.Name] = make(map[string]cty.Value)
		}
		names[name.Name][attr.Name] = ev.configuredAttribute(typeName, name.Name, attr.Name)
	}
	objs := make(map[string]cty.Value, len(names))
	for name, attrs := range names {
		objs[name] = cty.ObjectVal(attrs)
	}
	return cty.ObjectVal(objs)
}

// configuredAttribute evaluates the argument attr that the resource
// typeName.name sets in its configuration, or returns an unknown value.
func (ev *Evaluator) configuredAttribute(typeName, name, attr string) cty.Value {
	if ev.host == nil || ev.host.Attribute == nil {
		return cty.DynamicVal
	}
	key := "resource " + typeName + "." + name + "." + attr
	if v, ok := ev.configured[key]; ok {
		return v
	}
	if ev.evaluating[key] {
		return cty.DynamicVal
	}
	ev.evaluating[key] = true
	v := ev.configuredAttributeUncached(typeName, name, attr)
	delete(ev.evaluating, key)
	ev.configured[key] = v
	return v
}

func (ev *Evaluator) configuredAttributeUncached(typeName, name, attr string) cty.Value {
	info, ok := ev.host.Attribute("resource", typeName, attr)
	if !ok || info.Computed {
		// a provider may plan another value for an optional and
		// computed argument (legacy providers normalize some)
		return cty.DynamicVal
	}
	block := ev.findBlock("resource", typeName, name)
	if block == nil || block.Body.Attributes["count"] != nil || block.Body.Attributes["for_each"] != nil {
		return cty.DynamicVal
	}
	a, ok := block.Body.Attributes[attr]
	if !ok || ignoresChanges(block, attr) {
		return cty.DynamicVal
	}
	r := ev.Eval(a.Expr, nil)
	if !r.IsKnown() {
		return cty.DynamicVal
	}
	if info.Sensitive {
		return r.Value.Mark(SensitiveMark)
	}
	return r.Value
}

// ignoresChanges reports whether the lifecycle block of a resource lists
// the argument attr in ignore_changes, or ignores all changes. After the
// first apply, OpenTofu then keeps the argument's value from the state,
// whatever the configuration says. An ignore_changes it cannot read
// counts as ignoring everything.
func ignoresChanges(block *hclsyntax.Block, attr string) bool {
	for _, b := range block.Body.Blocks {
		if b.Type != "lifecycle" {
			continue
		}
		a, ok := b.Body.Attributes["ignore_changes"]
		if !ok {
			continue
		}
		if t, diags := hcl.AbsTraversalForExpr(a.Expr); !diags.HasErrors() && len(t) == 1 && t.RootName() == "all" {
			return true
		}
		tuple, ok := a.Expr.(*hclsyntax.TupleConsExpr)
		if !ok {
			return true
		}
		for _, item := range tuple.Exprs {
			if t, diags := hcl.AbsTraversalForExpr(item); !diags.HasErrors() {
				if t.RootName() == attr {
					return true
				}
				continue
			}
			// the legacy quoted form, such as "tags" or "tags.Name"
			v, diags := item.Value(nil)
			if diags.HasErrors() || !v.IsKnown() || v.IsNull() || v.Type() != cty.String {
				return true
			}
			if s := v.AsString(); s == attr || strings.HasPrefix(s, attr+".") || strings.HasPrefix(s, attr+"[") {
				return true
			}
		}
	}
	return false
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
			if v, ok := ev.evalModuleInstances(name); ok {
				calls[name] = v
			}
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
	if ev.host == nil || ev.depth >= maxModuleDepth {
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
		if r.IsKnown() {
			r.Value = o.mark(r.Value)
		}
		outs[outName] = r
	}
	ev.moduleOutputs[name] = outs
	return outs, true
}

// maxModuleInstances caps the instances of a module call whose outputs
// are evaluated.
const maxModuleInstances = 32

// evalModuleInstances evaluates the outputs of every instance of a module
// call with for_each or count: an object by instance key for for_each, a
// tuple for count. Unknown outputs are unknown values in it.
func (ev *Evaluator) evalModuleInstances(name string) (cty.Value, bool) {
	if v, ok := ev.moduleInstances[name]; ok {
		return v, v != cty.NilVal
	}
	ev.moduleInstances[name] = cty.NilVal
	if ev.host == nil || ev.depth >= maxModuleDepth {
		return cty.NilVal, false
	}
	block := ev.findModuleCall(name)
	if block == nil {
		return cty.NilVal, false
	}
	meta, r, insts, ok := ev.Instances(block)
	if !ok || !r.IsKnown() || r.IsSensitive() || len(insts) > maxModuleInstances {
		return cty.NilVal, false
	}
	mc, ok := ev.resolveModuleCall(block, *ev.host)
	if !ok {
		return cty.NilVal, false
	}
	outputs := mc.child.outputs()
	byKey := make(map[string]cty.Value, len(insts))
	list := make([]cty.Value, 0, len(insts))
	for _, inst := range insts {
		child := ev.childEvaluatorFor(mc, inst.Each)
		vals := make(map[string]cty.Value, len(outputs))
		for outName, o := range outputs {
			val := cty.DynamicVal
			if o.expr != nil {
				if r := child.Eval(o.expr, nil); r.IsKnown() {
					val = o.mark(r.Value)
				}
			}
			vals[outName] = val
		}
		obj := objectOrEmpty(vals)
		if meta == "count" {
			list = append(list, obj)
		} else if inst.Key.Type() == cty.String {
			byKey[inst.Key.AsString()] = obj
		}
	}
	v := objectOrEmpty(byKey)
	if meta == "count" {
		v = cty.EmptyTupleVal
		if len(list) > 0 {
			v = cty.TupleVal(list)
		}
	}
	ev.moduleInstances[name] = v
	return v, true
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
				if r.Kind == NotEvaluated || r.Kind == Rejected {
					// the child's own reason, such as an unsupported function
					// or a value that fails validation
					return Result{Kind: r.Kind, Reason: r.Reason}, true
				}
				return Result{Kind: r.Kind, Reason: TraversalString(t), Detail: r.Detail}, true
			}
			if diags.HasErrors() {
				continue
			}
			if ev.moduleOutputs[call] == nil {
				// The call's outputs were not evaluated: it has too many
				// or unknown instances, or its module could not be loaded.
				reason := "module." + call + " is not evaluated statically"
				if v := ev.moduleInstances[call]; v != cty.NilVal {
					reason = "`" + TraversalString(t) + "` is not known statically"
				} else if block := ev.findModuleCall(call); block != nil && (block.Body.Attributes["for_each"] != nil || block.Body.Attributes["count"] != nil) {
					reason = "module." + call + " has several instances (for_each or count), which are not evaluated statically"
				}
				return Result{Kind: NotEvaluated, Reason: reason}, true
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
			return ev.dataCause(t), true
		}
		if ev.ignoredChange(t) {
			return Result{Kind: FromState, Reason: TraversalString(t)}, true
		}
		return Result{Kind: AfterApply, Reason: TraversalString(t)}, true
	}

	var fn, atPlan string
	hclsyntax.VisitAll(syntaxNode(expr), func(n hclsyntax.Node) hcl.Diagnostics {
		if call, ok := n.(*hclsyntax.FunctionCallExpr); ok && earlydecoder.ImpureFunctions[call.Name] {
			switch {
			case call.Name == "plantimestamp":
				// plantimestamp is fixed when the plan is made.
				if atPlan == "" {
					atPlan = call.Name
				}
			case fn == "":
				fn = call.Name
			}
		}
		return nil
	})
	if fn != "" {
		return Result{Kind: AfterApply, Reason: fn + "()"}, true
	}
	if atPlan != "" {
		return Result{Kind: AtPlan, Reason: atPlan + "()"}, true
	}
	return Result{}, false
}

// ignoredChange reports whether t refers to a configured argument of a
// resource whose lifecycle ignore_changes lists it.
func (ev *Evaluator) ignoredChange(t hcl.Traversal) bool {
	name, ok1 := attrStep(t, 1)
	attr, ok2 := attrStep(t, 2)
	if !ok1 || !ok2 {
		return false
	}
	if _, ok := t[2].(hcl.TraverseAttr); !ok {
		return false
	}
	block := ev.findBlock("resource", t.RootName(), name)
	if block == nil || block.Body.Attributes[attr] == nil {
		return false
	}
	return ignoresChanges(block, attr)
}

// dataCause says when the data source that t refers to is read: during
// the plan, or during apply when its depends_on names a managed resource
// or module, or when one of its arguments is only known after apply.
func (ev *Evaluator) dataCause(t hcl.Traversal) Result {
	ref := TraversalString(t)
	typeName, ok1 := attrStep(t, 1)
	name, ok2 := attrStep(t, 2)
	if !ok1 || !ok2 {
		return Result{Kind: AtPlan, Reason: ref}
	}
	r := ev.DataReadTiming(typeName, name)
	switch r.Kind {
	case AtPlan:
		return Result{Kind: AtPlan, Reason: ref}
	case AfterApply:
		return Result{Kind: AfterApply, Reason: ref, Detail: r.Detail}
	}
	return r
}

// DataReadTiming says when OpenTofu reads the data source with the given
// type and name. Kind is AtPlan when it is read during the plan and
// AfterApply, with Detail saying why, when it is read during apply. Any
// other kind is the result of an argument that is not known statically.
func (ev *Evaluator) DataReadTiming(typeName, name string) Result {
	addr := "data." + typeName + "." + name
	if r, ok := ev.dataTiming[addr]; ok {
		return r
	}
	block := ev.findBlock("data", typeName, name)
	if block == nil || ev.readingData[addr] {
		return Result{Kind: AtPlan}
	}
	ev.readingData[addr] = true
	r := ev.dataReadTiming(block, addr)
	delete(ev.readingData, addr)
	ev.dataTiming[addr] = r
	return r
}

func (ev *Evaluator) dataReadTiming(block *hclsyntax.Block, addr string) Result {
	if attr, ok := block.Body.Attributes["depends_on"]; ok {
		for _, dep := range attr.Expr.Variables() {
			switch dep.RootName() {
			case "data":
				depType, _ := attrStep(dep, 1)
				depName, _ := attrStep(dep, 2)
				if r := ev.DataReadTiming(depType, depName); r.Kind == AfterApply {
					return Result{Kind: AfterApply, Detail: fmt.Sprintf("`%s` is read during apply: its `depends_on` names `%s`, which is read during apply", addr, TraversalString(dep))}
				}
			case "var", "local", "path", "terraform", "each", "count", "self":
			default:
				return Result{Kind: AfterApply, Detail: fmt.Sprintf("`%s` is read during apply whenever `%s` in its `depends_on` has changes, which includes the first apply", addr, TraversalString(dep))}
			}
		}
	}

	var cause *Result
	var visit func(body *hclsyntax.Body)
	visit = func(body *hclsyntax.Body) {
		for _, attrName := range mapKeys(body.Attributes) {
			switch attrName {
			case "count", "for_each", "depends_on", "provider":
				continue
			}
			if cause != nil {
				return
			}
			r := ev.Eval(body.Attributes[attrName].Expr, nil)
			if !r.IsKnown() && r.Kind != AtPlan {
				cause = &r
			}
		}
		for _, b := range body.Blocks {
			if b.Type != "lifecycle" && cause == nil {
				visit(b.Body)
			}
		}
	}
	visit(block.Body)
	if cause == nil {
		return Result{Kind: AtPlan}
	}
	if cause.Kind == AfterApply {
		return Result{Kind: AfterApply, Detail: fmt.Sprintf("`%s` is read during apply: its arguments depend on `%s`", addr, cause.Reason)}
	}
	return *cause
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
		if s.Key.Type() == cty.String && s.Key.IsKnown() && !s.Key.IsNull() && !s.Key.IsMarked() {
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
