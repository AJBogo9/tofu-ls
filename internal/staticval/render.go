// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package staticval

import (
	"strings"
	"unicode/utf8"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

const (
	unknownText   = "(known after apply)"
	sensitiveText = "(sensitive)"
	ephemeralText = "(ephemeral value)"
)

// redactedText is what is shown instead of a marked value: "(ephemeral
// value)", as tofu console prints it, when any part is ephemeral, and
// "(sensitive)" otherwise.
func redactedText(v cty.Value) string {
	if _, marks := v.UnmarkDeep(); marks.Has(EphemeralMark) {
		return ephemeralText
	}
	return sensitiveText
}

// textBuilder is a strings.Builder that stops taking text once it holds
// limit bytes (0 means no limit), so that rendering a huge value costs no
// more than the part that is shown.
type textBuilder struct {
	strings.Builder
	limit int
}

func (b *textBuilder) full() bool {
	return b.limit > 0 && b.Len() >= b.limit
}

// FormatValue renders a value as HCL, over several lines when it does not
// fit on one. Unknown parts render as "(known after apply)" and sensitive
// parts as "(sensitive)", so the text never shows a secret.
func FormatValue(v cty.Value) string {
	var b textBuilder
	writeValue(&b, v, "")
	return b.String()
}

// maxRenderBytes bounds the text rendered for a hover value: more than
// maxValueChars runes of text, so capValueText still has the lines it
// keeps, but not the whole of a value with millions of elements.
const maxRenderBytes = 4*maxValueChars + 1024

// formatValueCapped is FormatValue that stops after maxRenderBytes and
// reports whether it did.
func formatValueCapped(v cty.Value) (string, bool) {
	b := textBuilder{limit: maxRenderBytes}
	writeValue(&b, v, "")
	return b.String(), b.full()
}

const inlineWidth = 60

func writeValue(b *textBuilder, v cty.Value, indent string) {
	if b.full() {
		return
	}
	if v.IsMarked() {
		b.WriteString(redactedText(v))
		return
	}
	if !v.IsKnown() {
		b.WriteString(unknownText)
		return
	}
	if v.IsNull() {
		b.WriteString("null")
		return
	}
	ty := v.Type()
	switch {
	case ty == cty.String:
		s := v.AsString()
		if strings.Contains(strings.TrimSuffix(s, "\n"), "\n") && writeHeredoc(b, s, indent) {
			return
		}
		writeQuoted(b, s)
		return
	case ty.IsPrimitiveType():
		b.WriteString(formatPrimitive(v))
		return
	}

	if one, cut := formatOneLineLimit(v, 4*inlineWidth+16); !cut && utf8.RuneCountInString(one)+len(indent) <= inlineWidth && !strings.Contains(one, "\n") && !hasNested(v) {
		b.WriteString(one)
		return
	}

	inner := indent + "  "
	switch {
	case ty.IsListType() || ty.IsSetType() || ty.IsTupleType():
		if v.LengthInt() == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for it := v.ElementIterator(); it.Next() && !b.full(); {
			_, e := it.Element()
			b.WriteString(inner)
			writeValue(b, e, inner)
			b.WriteString(",\n")
		}
		b.WriteString(indent)
		b.WriteString("]")
	case ty.IsMapType() || ty.IsObjectType():
		if v.LengthInt() == 0 {
			b.WriteString("{}")
			return
		}
		// Keys align to the longest one among those that can be shown.
		keys := make([]string, 0, min(v.LengthInt(), 1024))
		width, keyBytes := 0, 0
		for it := v.ElementIterator(); it.Next(); {
			if b.limit > 0 && keyBytes > b.limit {
				break
			}
			k, _ := it.Element()
			key := formatKey(k.AsString())
			keys = append(keys, key)
			keyBytes += len(key)
			if n := utf8.RuneCountInString(key); n > width {
				width = n
			}
		}
		b.WriteString("{\n")
		i := 0
		for it := v.ElementIterator(); it.Next() && i < len(keys) && !b.full(); {
			_, e := it.Element()
			key := keys[i]
			i++
			b.WriteString(inner)
			b.WriteString(key)
			b.WriteString(strings.Repeat(" ", width-utf8.RuneCountInString(key)))
			b.WriteString(" = ")
			writeValue(b, e, inner)
			b.WriteString("\n")
		}
		b.WriteString(indent)
		b.WriteString("}")
	default:
		b.WriteString(formatOneLine(v))
	}
}

// hasNested reports whether a collection contains another non-empty
// collection, which reads better over several lines.
func hasNested(v cty.Value) bool {
	if v.IsMarked() || !v.IsKnown() || v.IsNull() {
		return false
	}
	ty := v.Type()
	if !ty.IsCollectionType() && !ty.IsObjectType() && !ty.IsTupleType() {
		return false
	}
	for it := v.ElementIterator(); it.Next(); {
		_, e := it.Element()
		if e.IsMarked() || !e.IsKnown() || e.IsNull() {
			continue
		}
		ety := e.Type()
		if (ety.IsObjectType() || ety.IsMapType()) && e.LengthInt() > 0 {
			return true
		}
	}
	return false
}

// writeHeredoc renders a multi-line string as an indented heredoc when
// one denotes exactly the same string, and reports whether it did. A
// heredoc always ends with a newline, <<- strips the indentation that
// its lines share, and a line holding only the delimiter ends it, so
// strings that differ in any of these ways are left to quoteString.
func writeHeredoc(b *textBuilder, s, indent string) bool {
	if !strings.HasSuffix(s, "\n") || strings.Contains(s, "\r") {
		return false
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	flush := false
	for _, line := range lines {
		if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			flush = true
		}
		if strings.HasPrefix(line, "\t") {
			// tabs and the added spaces do not strip alike
			return false
		}
	}
	if !flush {
		return false
	}
	delim := ""
	for _, candidate := range []string{"EOT", "EOF", "END"} {
		clash := false
		for _, line := range lines {
			if strings.TrimSpace(line) == candidate {
				clash = true
				break
			}
		}
		if !clash {
			delim = candidate
			break
		}
	}
	if delim == "" {
		return false
	}
	b.WriteString("<<-" + delim + "\n")
	for _, line := range lines {
		if b.full() {
			return true
		}
		if line != "" {
			// a heredoc is a template: escape interpolation markers
			line = strings.ReplaceAll(line, "${", "$${")
			line = strings.ReplaceAll(line, "%{", "%%{")
			b.WriteString(indent + "  " + line)
		}
		b.WriteString("\n")
	}
	b.WriteString(indent + delim)
	return true
}

// formatOneLine renders a value on a single line.
func formatOneLine(v cty.Value) string {
	var b textBuilder
	writeOneLine(&b, v)
	return b.String()
}

// formatOneLineLimit is formatOneLine that stops after about limit bytes
// and reports whether it did.
func formatOneLineLimit(v cty.Value, limit int) (string, bool) {
	b := textBuilder{limit: limit}
	writeOneLine(&b, v)
	return b.String(), b.full()
}

func writeOneLine(b *textBuilder, v cty.Value) {
	if b.full() {
		return
	}
	if v.IsMarked() {
		b.WriteString(redactedText(v))
		return
	}
	if !v.IsKnown() {
		b.WriteString(unknownText)
		return
	}
	if v.IsNull() {
		b.WriteString("null")
		return
	}
	ty := v.Type()
	switch {
	case ty == cty.String:
		writeQuoted(b, v.AsString())
	case ty.IsPrimitiveType():
		b.WriteString(formatPrimitive(v))
	case ty.IsListType() || ty.IsSetType() || ty.IsTupleType():
		b.WriteString("[")
		i := 0
		for it := v.ElementIterator(); it.Next() && !b.full(); {
			_, e := it.Element()
			if i > 0 {
				b.WriteString(", ")
			}
			writeOneLine(b, e)
			i++
		}
		b.WriteString("]")
	case ty.IsMapType() || ty.IsObjectType():
		if v.LengthInt() == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{ ")
		i := 0
		for it := v.ElementIterator(); it.Next() && !b.full(); {
			k, e := it.Element()
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(formatKey(k.AsString()))
			b.WriteString(" = ")
			writeOneLine(b, e)
			i++
		}
		b.WriteString(" }")
	default:
		b.WriteString(ty.FriendlyName())
	}
}

// FormatCompact renders a value on one line in at most max characters
// (runes), for inlay hints and short summaries. Collections that do not
// fit keep their first elements and say how many are left out; long
// strings are cut with an ellipsis.
func FormatCompact(v cty.Value, max int) string {
	limit := 4*max + 16
	full, cut := formatOneLineLimit(v, limit)
	if !cut && utf8.RuneCountInString(full) <= max {
		return full
	}
	if v.IsMarked() || !v.IsKnown() || v.IsNull() {
		return truncate(full, max)
	}
	ty := v.Type()
	switch {
	case ty.IsListType() || ty.IsSetType() || ty.IsTupleType():
		return compactSeq(v, max, "[", "]", func(_ cty.Value, e cty.Value) string {
			s, _ := formatOneLineLimit(e, limit)
			return s
		})
	case ty.IsMapType() || ty.IsObjectType():
		return compactSeq(v, max, "{ ", " }", func(k cty.Value, _ cty.Value) string { return formatKey(k.AsString()) + " = …" })
	case ty == cty.String:
		return truncate(full[:len(full)-1], max-1) + `"`
	}
	return truncate(full, max)
}

func compactSeq(v cty.Value, max int, open, close string, item func(k, e cty.Value) string) string {
	total := v.LengthInt()
	parts := []string{}
	used := utf8.RuneCountInString(open) + utf8.RuneCountInString(close)
	i := 0
	for it := v.ElementIterator(); it.Next(); {
		k, e := it.Element()
		s := item(k, e)
		rest := total - i - 1
		suffix := 0
		if rest > 0 {
			suffix = len(", … N more")
		}
		if used+utf8.RuneCountInString(s)+2+suffix > max {
			break
		}
		parts = append(parts, s)
		used += utf8.RuneCountInString(s) + 2
		i++
	}
	if len(parts) < total {
		more := "…"
		if len(parts) > 0 {
			more = "… " + itoa(total-len(parts)) + " more"
		} else {
			more = itoa(total) + " items"
			if total == 1 {
				more = "1 item"
			}
		}
		parts = append(parts, more)
	}
	return open + strings.Join(parts, ", ") + close
}

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	if max < 1 {
		return "…"
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

func itoa(n int) string {
	return cty.NumberIntVal(int64(n)).AsBigFloat().Text('f', -1)
}

func formatPrimitive(v cty.Value) string {
	switch v.Type() {
	case cty.Number:
		return v.AsBigFloat().Text('f', -1)
	case cty.Bool:
		if v.True() {
			return "true"
		}
		return "false"
	}
	return v.GoString()
}

// quoteString renders a string as an HCL quoted template literal.
func quoteString(s string) string {
	var b textBuilder
	writeQuoted(&b, s)
	return b.String()
}

// writeQuoted writes s as an HCL quoted template literal, stopping early
// when b is full.
func writeQuoted(b *textBuilder, s string) {
	b.WriteByte('"')
	for i, r := range s {
		if b.full() {
			return
		}
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '$', '%':
			b.WriteRune(r)
			if i+1 < len(s) && s[i+1] == '{' {
				b.WriteRune(r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

func formatKey(k string) string {
	if hclsyntax.ValidIdentifier(k) {
		return k
	}
	return quoteString(k)
}
