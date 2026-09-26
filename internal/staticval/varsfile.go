// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package staticval

import (
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

// VarsFileHover answers a hover on an assignment in a tfvars file: which
// variable it sets, whether the value matches the variable's type, and
// whether another file overrides it. file is the parsed tfvars file and
// filename its base name.
func (ev *Evaluator) VarsFileHover(filename string, file *hcl.File, pos hcl.Pos) (*Hover, bool) {
	if file == nil {
		return nil, false
	}
	attrs, _ := file.Body.JustAttributes()
	var attr *hcl.Attribute
	for _, a := range attrs {
		if a.Range.ContainsPos(pos) {
			attr = a
			break
		}
	}
	if attr == nil {
		return nil, false
	}

	v, ok := ev.vars[attr.Name]
	if !ok {
		return &Hover{
			Content: fmt.Sprintf("`%s`\n\nNo variable `%s` is declared in this module, so OpenTofu ignores this value (with a warning).", attr.Name, attr.Name),
			Range:   attr.NameRange,
		}, true
	}

	var b strings.Builder
	typeInline, typeBlock := typeDisplay(v)
	fmt.Fprintf(&b, "`var.%s`", v.Name)
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
	var flags []string
	if !v.HasDefault {
		flags = append(flags, "required")
	}
	if v.Sensitive {
		flags = append(flags, "sensitive")
	}
	if v.Ephemeral {
		flags = append(flags, "ephemeral")
	}
	if len(flags) > 0 {
		b.WriteString(strings.Join(flags, " · "))
		b.WriteString("\n\n")
	}

	val, diags := attr.Expr.Value(nil)
	switch {
	case diags.HasErrors():
		fmt.Fprintf(&b, "The value is not a constant, which tfvars files require (%s).\n\n", diagSummary(diags))
	default:
		if converted, err := v.convert(val); err != nil {
			fmt.Fprintf(&b, "**Does not match the type:** %s\n\n", err)
		} else if failures := ev.Validate(v.Name, converted); len(failures) > 0 {
			fmt.Fprintf(&b, "**Fails %s**, so OpenTofu refuses to plan with it:\n\n", plural(len(failures), "a validation rule", "validation rules"))
			b.WriteString(failureText(failures))
		} else {
			b.WriteString("Matches the variable's type.\n\n")
		}
	}

	if !IsAutoVarsFile(filename) {
		fmt.Fprintf(&b, "OpenTofu does not load `%s` by itself: it applies only with `-var-file=%s`, and then overrides the default and every automatically loaded tfvars file.\n\n", filename, filename)
	} else if ev.mod.RootPath != "" {
		b.WriteString("This is a child module: OpenTofu reads tfvars files only in the root module, so this value is ignored.\n\n")
	} else if ev.refused.Kind == Rejected {
		fmt.Fprintf(&b, "**No value is used**: %s.\n\n", ev.refused.Reason)
	} else {
		_, winner, ok := v.Effective()
		switch {
		case ok && winner == filename:
			b.WriteString("**This value is used**: no automatically loaded file after it sets the variable.\n\n")
		case ok && winner == "default" && val.IsNull() && !v.Nullable:
			if v.redacted() {
				b.WriteString("The variable is not nullable, so OpenTofu replaces this `null` with the default.\n\n")
			} else {
				winVal, _, _ := v.Effective()
				fmt.Fprintf(&b, "The variable is not nullable, so OpenTofu replaces this `null` with the default, `%s`.\n\n", FormatCompact(winVal, 40))
			}
		case ok && winner == "default":
			// a later file's value did not convert, so the default wins
			if v.redacted() {
				b.WriteString("**Overridden** by the default.\n\n")
			} else {
				winVal, _, _ := v.Effective()
				fmt.Fprintf(&b, "**Overridden** by the default, `%s`.\n\n", FormatCompact(winVal, 40))
			}
		case ok:
			if v.redacted() {
				fmt.Fprintf(&b, "**Overridden** by `%s`.\n\n", winner)
			} else {
				winVal, _, _ := v.Effective()
				fmt.Fprintf(&b, "**Overridden** by `%s`, which sets `%s`.\n\n", winner, FormatCompact(winVal, 40))
			}
		}
		b.WriteString("_May be overridden by `-var` or `-var-file`._\n\n")
	}
	fmt.Fprintf(&b, "_Declared in `%s`_", v.File)
	return &Hover{Content: b.String(), Range: attr.Range}, true
}
