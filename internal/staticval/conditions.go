// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package staticval

import (
	"path/filepath"
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// VariableFailure is a value that a variable gets without a plan and that
// fails the variable's validation rules, so that OpenTofu refuses to plan.
type VariableFailure struct {
	Variable string
	// Range is where the value is written: the tfvars value, the module
	// call argument or the default. For a TF_VAR_ environment variable it
	// is the variable's declaration.
	Range hcl.Range
	// Source is the tfvars file name, TF_VAR_<name>, or "default", for a
	// value of this module's own variable; "argument" for the value that a
	// module call of this module passes to the called module's variable.
	Source string
	// Kind is the kind of Source for a tfvars file or TF_VAR_ variable.
	Kind AssignmentKind
	// Calls are the module calls (with their instance keys) that pass the
	// value, for an argument or for a default that callers leave unset;
	// for example `module.app["a"]`.
	Calls []string
	// Rules are the declarations of the rules, in the called module for an
	// argument.
	Failures []ValidationFailure
}

// maxConditionInstances caps the instances of one block whose conditions
// are evaluated, and of one module call whose arguments are validated.
const maxConditionInstances = 64

// VariableFailures returns the values that fail validation rules:
//
//   - in a root module, the value each variable gets from TF_VAR_, the
//     tfvars and -var-file files or its default (see Inputs);
//   - in a child module, the default of each variable that a caller
//     leaves unset;
//   - for every module call of this module that env resolves, each
//     argument, checked against the called module's rules.
//
// Rules are checked with every value the module or call gives at once,
// so a rule may refer to other variables. Only failures that are certain
// are returned: a rule that is not known statically is skipped.
func (ev *Evaluator) VariableFailures(env Env) []VariableFailure {
	var out []VariableFailure
	if ev.mod.RootPath == "" {
		out = append(out, ev.rootFailures()...)
	} else {
		out = append(out, ev.defaultFailures()...)
	}
	out = append(out, ev.argumentFailures(env)...)
	return out
}

func (ev *Evaluator) rootFailures() []VariableFailure {
	if ev.refused.Kind == Rejected {
		return nil
	}
	var out []VariableFailure
	for _, name := range mapKeys(ev.failures) {
		v := ev.vars[name]
		_, a, ok := v.effective()
		if !ok {
			continue
		}
		f := VariableFailure{Variable: name, Failures: ev.failures[name]}
		switch {
		case a == nil:
			f.Source, f.Range = "default", v.DefaultRange
		default:
			f.Source, f.Kind, f.Range = a.File, a.Kind, a.ValueRange
			if a.Kind == FromEnvironment || a.Kind == FromCommandLine {
				f.Range = v.DeclRange
			}
		}
		out = append(out, f)
	}
	return out
}

// defaultFailures checks, for each caller of this child module, the
// defaults of the variables it leaves unset, together with the values it
// passes.
func (ev *Evaluator) defaultFailures() []VariableFailure {
	byVar := make(map[string]*VariableFailure)
	parents := make(map[*Module]*Evaluator)
	for _, c := range ev.mod.Callers {
		parentEv, ok := parents[c.Parent]
		if !ok {
			parentEv = ev.newEvaluator(c.Parent)
			parents[c.Parent] = parentEv
		}
		block := parentEv.findModuleCall(c.Name)
		if block == nil {
			continue
		}
		for _, inst := range parentEv.callInstances(block) {
			vals := make(map[string]cty.Value, len(ev.vars))
			var unset []string
			for name, v := range ev.vars {
				cv := parentEv.callValueForInstance(block, v, ev.mod.Path, inst.each)
				if cv.Result.IsKnown() {
					vals[name] = cv.Result.Value
				}
				if (!cv.Set || cv.NullReplaced) && v.HasDefault && len(v.Rules) > 0 && cv.Result.IsKnown() {
					unset = append(unset, name)
				}
			}
			if len(unset) == 0 {
				continue
			}
			sort.Strings(unset)
			for name, failures := range ev.validateInputs(vals, unset) {
				f, ok := byVar[name]
				if !ok {
					f = &VariableFailure{Variable: name, Source: "default", Range: ev.vars[name].DefaultRange, Failures: failures}
					byVar[name] = f
				}
				f.Calls = append(f.Calls, "module."+c.Name+inst.key)
			}
		}
	}
	var out []VariableFailure
	for _, name := range mapKeys(byVar) {
		out = append(out, *byVar[name])
	}
	return out
}

// argumentFailures checks the arguments of every module call of this
// module against the rules of the called module's variables.
func (ev *Evaluator) argumentFailures(env Env) []VariableFailure {
	if env.LoadModule == nil {
		return nil
	}
	var out []VariableFailure
	for _, block := range ev.moduleBlocks() {
		mc, ok := ev.resolveModuleCall(block, env)
		if !ok {
			continue
		}
		child := ev.newEvaluator(mc.child)
		var names []string
		for name, v := range child.vars {
			if _, set := block.Body.Attributes[name]; set && len(v.Rules) > 0 {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			continue
		}
		sort.Strings(names)
		byVar := make(map[string]*VariableFailure)
		for _, inst := range ev.callInstances(block) {
			vals := make(map[string]cty.Value, len(child.vars))
			var check []string
			for name, v := range child.vars {
				cv := ev.callValueForInstance(block, v, mc.child.Path, inst.each)
				if cv.Result.IsKnown() {
					vals[name] = cv.Result.Value
				}
			}
			for _, name := range names {
				if _, ok := vals[name]; ok {
					check = append(check, name)
				}
			}
			for name, failures := range child.validateInputs(vals, check) {
				f, ok := byVar[name]
				if !ok {
					f = &VariableFailure{Variable: name, Source: "argument", Range: block.Body.Attributes[name].Expr.Range(), Failures: failures}
					byVar[name] = f
				}
				f.Calls = append(f.Calls, "module."+block.Labels[0]+inst.key)
			}
		}
		for _, name := range mapKeys(byVar) {
			out = append(out, *byVar[name])
		}
	}
	return out
}

// moduleBlocks returns the module calls of the module's native syntax
// files, in the order OpenTofu reads the files. Calls declared in an
// override file, or overridden by one, are left out, since the override
// may replace their arguments.
func (ev *Evaluator) moduleBlocks() []*hclsyntax.Block {
	overridden := ev.overriddenBlocks()
	var blocks []*hclsyntax.Block
	for _, fname := range ev.mod.sortedFileNames() {
		if IsOverrideFile(fname) {
			continue
		}
		body, ok := ev.mod.Files[fname].Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		for _, block := range body.Blocks {
			if block.Type == "module" && len(block.Labels) == 1 && !overridden[blockKey(block)] {
				blocks = append(blocks, block)
			}
		}
	}
	return blocks
}

// overriddenBlocks returns the keys (see blockKey) of the blocks that an
// override file declares.
func (ev *Evaluator) overriddenBlocks() map[string]bool {
	keys := make(map[string]bool)
	for _, fname := range ev.mod.sortedFileNames() {
		if !IsOverrideFile(fname) {
			continue
		}
		content, _, _ := ev.mod.Files[fname].Body.PartialContent(&hcl.BodySchema{
			Blocks: []hcl.BlockHeaderSchema{
				{Type: "module", LabelNames: []string{"name"}},
				{Type: "resource", LabelNames: []string{"type", "name"}},
				{Type: "data", LabelNames: []string{"type", "name"}},
				{Type: "output", LabelNames: []string{"name"}},
				{Type: "check", LabelNames: []string{"name"}},
			},
		})
		if content == nil {
			continue
		}
		for _, b := range content.Blocks {
			keys[b.Type+"\x00"+joinLabels(b.Labels)] = true
		}
	}
	return keys
}

func blockKey(block *hclsyntax.Block) string {
	return block.Type + "\x00" + joinLabels(block.Labels)
}

func joinLabels(labels []string) string {
	s := ""
	for i, l := range labels {
		if i > 0 {
			s += "\x00"
		}
		s += l
	}
	return s
}

// callInstance is one instance of a module call or resource, with the
// each or count values of the instance and its key as written in an
// address, such as `["a"]` or `[0]` ("" without for_each or count).
type callInstance struct {
	each map[string]cty.Value
	key  string
}

// callInstances returns the instances of a block: one without for_each
// or count, else each known instance. None when the instances are not
// known, or are too many.
func (ev *Evaluator) callInstances(block *hclsyntax.Block) []callInstance {
	meta, r, insts, ok := ev.Instances(block)
	if !ok {
		return []callInstance{{}}
	}
	if meta == "" || !r.IsKnown() || r.IsSensitive() || len(insts) > maxConditionInstances {
		return nil
	}
	out := make([]callInstance, 0, len(insts))
	for _, inst := range insts {
		out = append(out, callInstance{each: inst.Each, key: "[" + FormatCompact(inst.Key, 40) + "]"})
	}
	return out
}

// validateInputs checks the values vals of the module's variables (the
// values one module call gives, arguments and defaults) against the
// rules of the variables names. Every value is set at once, so that rules
// may refer to other variables; a variable missing from vals is unknown.
func (ev *Evaluator) validateInputs(vals map[string]cty.Value, names []string) map[string][]ValidationFailure {
	saved := ev.varValues
	ev.varValues = make(map[string]cty.Value, len(ev.vars))
	for name, v := range ev.vars {
		val, ok := vals[name]
		if !ok || val == cty.NilVal {
			val = cty.DynamicVal
		}
		ev.varValues[name] = v.markValue(val)
	}
	defer func() { ev.varValues = saved }()
	if len(names) == 0 {
		return nil
	}
	all := ev.validateAll(mapKeys(ev.vars))
	failures := make(map[string][]ValidationFailure)
	for _, name := range names {
		if f, ok := all[name]; ok {
			failures[name] = f
		}
	}
	return failures
}

// ConditionFailure is a precondition, postcondition or check assertion
// whose condition is false, or fails with an error, for values known
// without a plan.
type ConditionFailure struct {
	// Kind is "precondition", "postcondition" or "assert".
	Kind string
	// BlockType is the type of the block that holds the condition:
	// "resource", "data", "output" or "check".
	BlockType string
	// Address is the block's address with the instance key, such as
	// `terraform_data.web["a"]`, `data.x.y`, `output.name` or `check.name`.
	Address string
	// Range is the condition expression.
	Range hcl.Range
	// Message is the rendered error_message, or the error when Err is set.
	Message string
	Err     bool
}

// ConditionFailures evaluates the preconditions and postconditions of
// resources, the preconditions of data sources and outputs, and the
// assertions of check blocks whose conditions are known without a plan
// (see evalCondition). A condition that refers to self, a resource, a
// data source or a module output is never evaluated. A block with
// for_each or count is evaluated per instance when its instances are
// known, and not at all otherwise. In a child module the conditions are
// evaluated only when a caller is known to create the module, with the
// values that every caller agrees on.
func (ev *Evaluator) ConditionFailures() []ConditionFailure {
	if !ev.instantiated(maxCallerDepth) {
		return nil
	}
	overridden := ev.overriddenBlocks()
	var out []ConditionFailure
	for _, fname := range ev.mod.sortedFileNames() {
		if IsOverrideFile(fname) {
			continue
		}
		body, ok := ev.mod.Files[fname].Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		for _, block := range body.Blocks {
			if overridden[blockKey(block)] {
				continue
			}
			switch {
			case (block.Type == "resource" || block.Type == "data") && len(block.Labels) == 2:
				addr := block.Labels[0] + "." + block.Labels[1]
				if block.Type == "data" {
					addr = "data." + addr
				}
				for _, inst := range ev.callInstances(block) {
					for _, lc := range block.Body.Blocks {
						if lc.Type != "lifecycle" {
							continue
						}
						for _, cb := range lc.Body.Blocks {
							// a data source is read during apply when its
							// arguments are unknown, and its postconditions
							// are checked only then
							if cb.Type == "precondition" || (cb.Type == "postcondition" && block.Type == "resource") {
								out = ev.appendCondition(out, cb, cb.Type, block.Type, addr+inst.key, inst.each)
							}
						}
					}
				}
			case block.Type == "output" && len(block.Labels) == 1:
				for _, cb := range block.Body.Blocks {
					if cb.Type == "precondition" {
						out = ev.appendCondition(out, cb, "precondition", "output", "output."+block.Labels[0], nil)
					}
				}
			case block.Type == "check" && len(block.Labels) == 1:
				for _, cb := range block.Body.Blocks {
					if cb.Type == "assert" {
						out = ev.appendCondition(out, cb, "assert", "check", "check."+block.Labels[0], nil)
					}
				}
			}
		}
	}
	return out
}

func (ev *Evaluator) appendCondition(out []ConditionFailure, cb *hclsyntax.Block, kind, blockType, addr string, each map[string]cty.Value) []ConditionFailure {
	cond, ok := cb.Body.Attributes["condition"]
	if !ok {
		return out
	}
	res, msg := ev.evalCondition(cond.Expr, each)
	switch res {
	case conditionFalse:
		var source string
		var expr hcl.Expression
		if m, ok := cb.Body.Attributes["error_message"]; ok {
			expr = m.Expr
			source = sourceText(ev.mod.Files[filepath.Base(m.Expr.Range().Filename)], m.Expr.Range())
		}
		msg = ev.renderMessage(expr, source, each)
	case conditionError:
	default:
		return out
	}
	return append(out, ConditionFailure{
		Kind:      kind,
		BlockType: blockType,
		Address:   addr,
		Range:     cond.Expr.Range(),
		Message:   msg,
		Err:       res == conditionError,
	})
}

// instantiated reports whether OpenTofu is known to create this module:
// it is a root module, or a caller that is itself created calls it
// without for_each or count, or with known instances.
func (ev *Evaluator) instantiated(depth int) bool {
	if ev.mod.RootPath == "" {
		return true
	}
	if depth <= 0 {
		return false
	}
	parents := make(map[*Module]*Evaluator)
	for _, c := range ev.mod.Callers {
		parentEv, ok := parents[c.Parent]
		if !ok {
			parentEv = ev.newEvaluator(c.Parent)
			parents[c.Parent] = parentEv
		}
		block := parentEv.findModuleCall(c.Name)
		if block == nil || len(parentEv.callInstances(block)) == 0 {
			continue
		}
		if parentEv.instantiated(depth - 1) {
			return true
		}
	}
	return false
}
