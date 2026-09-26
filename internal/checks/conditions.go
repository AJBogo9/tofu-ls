// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package checks

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	"github.com/opentofu/tofu-ls/internal/staticval"
	"github.com/opentofu/tofu-ls/internal/uri"
)

// conditions reports what OpenTofu rejects once the values are known,
// wherever every value involved is known without a plan (see package
// staticval):
//
//   - a value that fails its variable's validation rules, on the tfvars
//     value, the module call argument or the default that supplies it
//     (an error: OpenTofu refuses to plan);
//   - a precondition or postcondition that fails (an error: the plan
//     fails);
//   - a check assertion that fails (a warning, as OpenTofu reports it).
//
// A condition that fails with an error for known values, for example a
// function error, is reported too, since OpenTofu reports it as well.
// Nothing is reported while a configuration file is broken.
func (c *checker) conditions() {
	ev := c.mod.Values
	if ev == nil || !c.complete {
		return
	}
	for _, f := range ev.VariableFailures(c.mod.ValuesEnv) {
		c.validationFailure(f)
	}
	for _, f := range ev.ConditionFailures() {
		c.conditionFailure(f)
	}
}

// relRange returns rng with its file name relative to the module
// directory, or false for a file outside it.
func (c *checker) relRange(rng hcl.Range) (hcl.Range, bool) {
	if rng.Filename == "" {
		return rng, false
	}
	name := rng.Filename
	if filepath.IsAbs(name) {
		rel, err := filepath.Rel(c.mod.Path, name)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return rng, false
		}
		name = rel
	}
	rng.Filename = filepath.ToSlash(name)
	return rng, true
}

func (c *checker) validationFailure(f staticval.VariableFailure) {
	rng, ok := c.relRange(f.Range)
	if !ok {
		return
	}
	var msgs []string
	var related []ilsp.RelatedLocation
	for _, fail := range f.Failures {
		msg := fail.Message
		switch {
		case fail.Err:
			msg = "the validation condition fails with an error for this value: " + msg
		case msg == "":
			msg = "the value fails a validation rule, which has no error_message"
		}
		msgs = append(msgs, msg)
		related = append(related, ilsp.RelatedLocation{
			URI:     ruleURI(c.mod.Path, fail.Range),
			Range:   fail.Range,
			Message: "validation rule of var." + f.Variable,
		})
	}
	detail := strings.Join(msgs, "\n")
	switch {
	case f.Source == "argument" && len(f.Calls) > 0 && strings.Contains(f.Calls[0], "["):
		detail = fmt.Sprintf("%s: %s", listCalls(f.Calls), detail)
	case f.Source == "default" && len(f.Calls) > 0:
		detail = fmt.Sprintf("%s (the default, which %s leaves unset)", detail, listCalls(f.Calls))
	case f.Kind == staticval.FromEnvironment:
		detail = fmt.Sprintf("the value of %s fails validation: %s", f.Source, detail)
	}
	data := map[string]interface{}{"variable": f.Variable}
	if f.Source == "argument" && len(f.Calls) > 0 {
		call := strings.TrimPrefix(f.Calls[0], "module.")
		if i := strings.Index(call, "["); i >= 0 {
			call = call[:i]
		}
		data["module"] = call
	}
	c.add(hcl.DiagError, "Invalid value for variable", detail, rng, ilsp.CodedDiagnostic{
		Code:    ilsp.CodeValidationFailed,
		Data:    data,
		Related: related,
	})
}

func (c *checker) conditionFailure(f staticval.ConditionFailure) {
	rng, ok := c.relRange(f.Range)
	if !ok {
		return
	}
	sev := hcl.DiagError
	var summary string
	switch {
	case f.Kind == "assert":
		sev = hcl.DiagWarning
		summary = "Check block assertion failed"
	case f.BlockType == "output":
		summary = "Module output value precondition failed"
	case f.Kind == "postcondition":
		summary = "Resource postcondition failed"
	default:
		summary = "Resource precondition failed"
	}
	detail := f.Message
	if f.Err {
		detail = "the condition fails with an error: " + detail
	} else if detail == "" {
		detail = "the condition is false, and it has no error_message"
	}
	if strings.HasSuffix(f.Address, "]") {
		detail = f.Address + ": " + detail
	}
	c.add(sev, summary, detail, rng, ilsp.CodedDiagnostic{
		Code: ilsp.CodeConditionFailed,
		Data: map[string]interface{}{"kind": f.Kind, "address": f.Address},
	})
}

// listCalls lists module call addresses, shortening a long list.
func listCalls(calls []string) string {
	const max = 3
	if len(calls) <= max {
		return strings.Join(calls, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(calls[:max], ", "), len(calls)-max)
}

// ruleURI returns the URI of the file of a validation rule, whose range
// file name is a path, absolute or relative to the module directory.
func ruleURI(modPath string, rng hcl.Range) string {
	name := rng.Filename
	if !filepath.IsAbs(name) {
		name = filepath.Join(modPath, filepath.FromSlash(name))
	}
	return uri.FromPath(name)
}
