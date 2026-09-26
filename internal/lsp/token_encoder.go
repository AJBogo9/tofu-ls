// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package lsp

import (
	"bytes"
	"unicode/utf8"

	"github.com/hashicorp/hcl-lang/lang"
	"github.com/hashicorp/hcl/v2"
	"github.com/opentofu/tofu-ls/internal/lsp/semtok"
	lsp "github.com/opentofu/tofu-ls/internal/protocol"
	"github.com/opentofu/tofu-ls/internal/source"
)

type TokenEncoder struct {
	Lines      source.Lines
	Tokens     []lang.SemanticToken
	ClientCaps lsp.SemanticTokensClientCapabilities

	// lastEncodedTokenIdx tracks index of the last encoded token
	// so we can account for any skipped tokens in calculating diff
	lastEncodedTokenIdx int
}

func (te *TokenEncoder) Encode() []uint32 {
	data := make([]uint32, 0)

	for i := range te.Tokens {
		data = append(data, te.encodeTokenOfIndex(i)...)
	}

	return data
}

func (te *TokenEncoder) encodeTokenOfIndex(i int) []uint32 {
	token := te.Tokens[i]

	tokenType, ok := te.resolveTokenType(token)
	if !ok {
		return []uint32{}
	}
	tokenTypeIdx := TokenTypesLegend(te.ClientCaps.TokenTypes).Index(tokenType)

	modifiers := te.resolveTokenModifiers(token.Modifiers)
	modifierBitMask := TokenModifiersLegend(te.ClientCaps.TokenModifiers).BitMask(modifiers)

	data := make([]uint32, 0)

	// Client may not support multiline tokens which would be indicated
	// via lsp.SemanticTokensCapabilities.MultilineTokenSupport
	// once it becomes available in gopls LSP structs.
	//
	// For now we just safely assume client does *not* support it.

	tokenLineDelta := token.Range.End.Line - token.Range.Start.Line

	previousLine := 0
	previousStartChar := 0
	if i > 0 {
		previousTokenStartLine := te.Tokens[te.lastEncodedTokenIdx].Range.Start.Line - 1
		previousLine = te.Tokens[te.lastEncodedTokenIdx].Range.End.Line - 1
		currentLine := te.Tokens[i].Range.End.Line - 1
		// If the previous token started on a line and ended on another one, there is a good chance
		// to be inside a heredoc string which contains also different types of tokens (eg: references).
		// If that happens, it's safe to assume that the previousStartChar is 0
		// since the previous token "continued" from one line to another, making the start of the current line also the
		// start of the previous token.
		// But if both, the previous token (its start and end) and the current token are on the same line, we want
		// to use the previous token start marker to be able to correctly indicate the current token deltaStartChar.
		if currentLine == previousLine && previousTokenStartLine == previousLine {
			previousStartChar = te.char(te.Tokens[te.lastEncodedTokenIdx].Range.Start)
		}
	}

	if tokenLineDelta == 0 || false /* te.clientCaps.MultilineTokenSupport */ {
		deltaLine := token.Range.Start.Line - 1 - previousLine
		tokenLength := te.char(token.Range.End) - te.char(token.Range.Start)
		deltaStartChar := te.char(token.Range.Start) - previousStartChar

		data = append(data, []uint32{
			uint32(deltaLine),
			uint32(deltaStartChar),
			uint32(tokenLength),
			uint32(tokenTypeIdx),
			uint32(modifierBitMask),
		}...)
	} else {
		// Add entry for each line of a multiline token
		for tokenLine := token.Range.Start.Line - 1; tokenLine <= token.Range.End.Line-1; tokenLine++ {
			deltaLine := tokenLine - previousLine

			deltaStartChar := 0
			if tokenLine == token.Range.Start.Line-1 {
				deltaStartChar = te.char(token.Range.Start) - previousStartChar
			}

			lineBytes := bytes.TrimRight(te.Lines[tokenLine].Bytes, "\n\r")
			length := utf16Len(lineBytes)

			if tokenLine == token.Range.End.Line-1 {
				length = te.char(token.Range.End)
			}

			data = append(data, []uint32{
				uint32(deltaLine),
				uint32(deltaStartChar),
				uint32(length),
				uint32(tokenTypeIdx),
				uint32(modifierBitMask),
			}...)

			previousLine = tokenLine
		}
	}

	te.lastEncodedTokenIdx = i

	return data
}

// char returns the character of pos in its line in UTF-16 code units, as
// LSP counts it, from the byte offset. HCL counts columns in grapheme
// clusters, which differ on lines with emoji or combining marks. It falls
// back to the column when the lines do not hold the position.
func (te *TokenEncoder) char(pos hcl.Pos) int {
	line := pos.Line - 1
	if line < 0 || line >= len(te.Lines) {
		return pos.Column - 1
	}
	offset := pos.Byte - te.Lines[line].Range.Start.Byte
	if offset < 0 || offset > len(te.Lines[line].Bytes) {
		return pos.Column - 1
	}
	return utf16Len(te.Lines[line].Bytes[:offset])
}

func utf16Len(b []byte) int {
	n := 0
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		b = b[size:]
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func (te *TokenEncoder) resolveTokenType(token lang.SemanticToken) (semtok.TokenType, bool) {
	switch token.Type {
	case lang.TokenBlockType:
		return te.firstSupportedTokenType(
			semtok.TokenType(lang.TokenBlockType), semtok.TokenTypeType)
	case lang.TokenBlockLabel:
		return te.firstSupportedTokenType(
			semtok.TokenType(lang.TokenBlockLabel), semtok.TokenTypeEnumMember)
	case lang.TokenAttrName:
		return te.firstSupportedTokenType(
			semtok.TokenType(lang.TokenAttrName), semtok.TokenTypeProperty)
	case lang.TokenBool:
		return te.firstSupportedTokenType(
			semtok.TokenType(lang.TokenBool), semtok.TokenTypeKeyword)
	case lang.TokenNumber:
		return te.firstSupportedTokenType(
			semtok.TokenType(lang.TokenNumber), semtok.TokenTypeNumber)
	case lang.TokenString:
		return te.firstSupportedTokenType(
			semtok.TokenType(lang.TokenString), semtok.TokenTypeString)
	case lang.TokenObjectKey:
		return te.firstSupportedTokenType(
			semtok.TokenType(lang.TokenObjectKey), semtok.TokenTypeParameter)
	case lang.TokenMapKey:
		return te.firstSupportedTokenType(
			semtok.TokenType(lang.TokenMapKey), semtok.TokenTypeParameter)
	case lang.TokenKeyword:
		return te.firstSupportedTokenType(
			semtok.TokenType(lang.TokenKeyword), semtok.TokenTypeVariable)
	case lang.TokenReferenceStep:
		return te.firstSupportedTokenType(
			semtok.TokenType(lang.TokenReferenceStep), semtok.TokenTypeVariable)
	case lang.TokenTypeComplex:
		return te.firstSupportedTokenType(
			semtok.TokenType(lang.TokenTypeComplex), semtok.TokenTypeFunction)
	case lang.TokenTypePrimitive:
		return te.firstSupportedTokenType(
			semtok.TokenType(lang.TokenTypePrimitive), semtok.TokenTypeKeyword)
	case lang.TokenFunctionName:
		return te.firstSupportedTokenType(
			semtok.TokenType(lang.TokenFunctionName), semtok.TokenTypeFunction)
	}

	return "", false
}

func (te *TokenEncoder) resolveTokenModifiers(tokModifiers []lang.SemanticTokenModifier) semtok.TokenModifiers {
	modifiers := make(semtok.TokenModifiers, 0)

	for _, modifier := range tokModifiers {
		if modifier == lang.TokenModifierDependent {
			if te.tokenModifierSupported(string(lang.TokenModifierDependent)) {
				modifiers = append(modifiers, semtok.TokenModifier(lang.TokenModifierDependent))
				continue
			}
			if te.tokenModifierSupported(string(semtok.TokenModifierDefaultLibrary)) {
				modifiers = append(modifiers, semtok.TokenModifierDefaultLibrary)
				continue
			}
			continue
		}

		if te.tokenModifierSupported(string(modifier)) {
			modifiers = append(modifiers, semtok.TokenModifier(modifier))
		}
	}

	return modifiers
}

func (te *TokenEncoder) firstSupportedTokenType(tokenTypes ...semtok.TokenType) (semtok.TokenType, bool) {
	for _, tokenType := range tokenTypes {
		if te.tokenTypeSupported(string(tokenType)) {
			return tokenType, true
		}
	}
	return "", false
}

func (te *TokenEncoder) tokenTypeSupported(tokenType string) bool {
	return sliceContains(te.ClientCaps.TokenTypes, tokenType)
}

func (te *TokenEncoder) tokenModifierSupported(tokenModifier string) bool {
	return sliceContains(te.ClientCaps.TokenModifiers, tokenModifier)
}

func sliceContains(slice []string, value string) bool {
	for _, val := range slice {
		if val == value {
			return true
		}
	}
	return false
}
