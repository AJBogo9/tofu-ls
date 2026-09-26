// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package jobs

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/hcl-lang/decoder"
	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	tfschema "github.com/opentofu/opentofu-schema/schema"
	idecoder "github.com/opentofu/tofu-ls/internal/decoder"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/ast"
	fdecoder "github.com/opentofu/tofu-ls/internal/features/terragrunt/decoder"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/state"
	"github.com/opentofu/tofu-ls/internal/job"
	ilsp "github.com/opentofu/tofu-ls/internal/lsp"
	globalAst "github.com/opentofu/tofu-ls/internal/tofu/ast"
	op "github.com/opentofu/tofu-ls/internal/tofu/module/operation"
)

// SchemaTerragruntValidation reports the blocks and attributes of the
// Terragrunt files of a directory that Terragrunt's configuration
// reference does not have. Terragrunt adds features often, so these are
// warnings: a newer Terragrunt may accept what the schema does not know.
func SchemaTerragruntValidation(ctx context.Context, store *state.TerragruntStore, dir string) error {
	record, err := store.TerragruntRecordByPath(dir)
	if err != nil {
		return err
	}

	// Avoid validation if it is already in progress or already finished
	if record.DiagnosticsState[globalAst.SchemaValidationSource] != op.OpStateUnknown && !job.IgnoreState(ctx) {
		return job.StateNotChangedErr{Dir: document.DirHandleFromPath(dir)}
	}

	err = store.SetDiagnosticsState(dir, globalAst.SchemaValidationSource, op.OpStateLoading)
	if err != nil {
		return err
	}

	d := decoder.NewDecoder(&fdecoder.PathReader{StateReader: store})
	d.SetContext(idecoder.DecoderContext(ctx))

	diags := make(ast.Diags)
	var rErr error
	for _, languageID := range []string{ilsp.Terragrunt.String(), ilsp.TerragruntStack.String()} {
		if !record.HasLanguage(languageID) {
			continue
		}
		pathDecoder, err := d.Path(lang.Path{Path: dir, LanguageID: languageID})
		if err != nil {
			rErr = errors.Join(rErr, err)
			continue
		}
		langDiags, err := pathDecoder.Validate(ctx)
		rErr = errors.Join(rErr, err)
		for name, fileDiags := range langDiags {
			fileDiags = withoutAttributeForms(fileDiags, record.ParsedFiles[ast.Filename(name)])
			diags[ast.Filename(name)] = asWarnings(fileDiags)
		}
	}

	sErr := store.UpdateDiagnostics(dir, globalAst.SchemaValidationSource, diags)
	if sErr != nil {
		return sErr
	}

	return rErr
}

// asWarnings turns the validators' errors into warnings which say what
// Terragrunt knows instead.
func asWarnings(diags hcl.Diagnostics) hcl.Diagnostics {
	out := make(hcl.Diagnostics, 0, len(diags))
	for _, diag := range diags {
		d := *diag
		d.Severity = hcl.DiagWarning
		if m := unexpectedRe.FindStringSubmatch(d.Detail); m != nil {
			kind := "attribute"
			if strings.HasPrefix(d.Summary, "Unexpected block") {
				kind = "block"
			}
			d.Summary = "Unknown Terragrunt " + kind
			d.Detail = fmt.Sprintf("Terragrunt %s has no %s %s here. It may be a typo, or new in a later "+
				"Terragrunt version.", tfschema.TerragruntVersion, kind, m[1])
		}
		out = append(out, &d)
	}
	return out
}

// unexpectedRe reads the name from the messages of
// validator.UnexpectedAttribute and validator.UnexpectedBlock.
var unexpectedRe = regexp.MustCompile(`^(?:An attribute named|Blocks of type) ("[^"]*") (?:is|are) not expected here`)

// withoutAttributeForms drops the diagnostics of blocks written as
// attributes (remote_state = {...}, generate = {...}), which Terragrunt
// accepts and the schema leaves out so that completion offers the blocks.
func withoutAttributeForms(diags hcl.Diagnostics, file *hcl.File) hcl.Diagnostics {
	if file == nil {
		return diags
	}
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return diags
	}
	forms := make(map[hcl.Pos]bool)
	_ = hclsyntax.VisitAll(body, func(node hclsyntax.Node) hcl.Diagnostics {
		if attr, ok := node.(*hclsyntax.Attribute); ok && tfschema.TerragruntAttributeForms[attr.Name] {
			forms[attr.SrcRange.Start] = true
		}
		return nil
	})
	out := make(hcl.Diagnostics, 0, len(diags))
	for _, diag := range diags {
		if diag.Subject != nil && forms[diag.Subject.Start] && strings.HasPrefix(diag.Summary, "Unexpected attribute") {
			continue
		}
		out = append(out, diag)
	}
	return out
}
