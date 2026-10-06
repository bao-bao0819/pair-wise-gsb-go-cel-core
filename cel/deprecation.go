// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cel

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/decls"
)

// DeprecationSeverity indicates how a deprecated overload usage is surfaced
// to the caller.
type DeprecationSeverity int

const (
	// DeprecationWarning indicates the usage is reported as a warning which
	// does not prevent the creation of a Program from the Ast.
	DeprecationWarning DeprecationSeverity = iota

	// DeprecationError indicates the usage is reported as an error. The
	// severity is informational; the DeprecationChecker never mutates or
	// rejects the Ast, so callers decide whether to block evaluation.
	DeprecationError
)

// String returns the human-readable name of the severity.
func (s DeprecationSeverity) String() string {
	if s == DeprecationError {
		return "error"
	}
	return "warning"
}

// DeprecatedOverload configures a single overload which is being phased out.
//
// The overload is identified by its exact overload ID as recorded in the
// type-checked Ast reference map, never by function name matching, so
// same-named overloads with new signatures remain unaffected.
type DeprecatedOverload struct {
	// OverloadID is the exact overload identifier to flag, e.g. "size_string".
	OverloadID string

	// Severity indicates whether usages are reported as warnings or errors.
	Severity DeprecationSeverity

	// Replacement is an advisory note describing the recommended
	// alternative. The text is returned verbatim within diagnostics and is
	// never used to rewrite the user expression.
	Replacement string
}

// SourceRange describes a range within the expression source text.
//
// Lines are 1-based and columns are 0-based, matching common.Location.
type SourceRange struct {
	StartLine   int
	StartColumn int
	EndLine     int
	EndColumn   int
}

// DeprecationDiagnostic describes a single call site which resolved to a
// deprecated overload.
type DeprecationDiagnostic struct {
	// OverloadID is the deprecated overload identifier which was matched.
	OverloadID string

	// FunctionName is the name of the function being called.
	FunctionName string

	// Signature is the resolved overload signature, e.g.
	// "string.legacy(int) -> string".
	Signature string

	// Severity indicates whether the usage is a warning or an error.
	Severity DeprecationSeverity

	// Replacement is the advisory replacement text from the rule.
	Replacement string

	// Ambiguous indicates the type-checker recorded multiple candidate
	// overloads for the call, so the deprecated overload may not be the
	// one invoked at runtime. Only overloads actually selected by the
	// checker are reported; unselected declarations are never listed.
	Ambiguous bool

	// Location is the source range of the call site, or nil when the Ast
	// carries no source mapping for the call. Locations are never
	// fabricated; macro-generated calls point back at the user-written
	// macro call whenever the parser recorded source metadata.
	Location *SourceRange
}

// String returns a human-readable rendering of the diagnostic.
func (d DeprecationDiagnostic) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: call to deprecated overload '%s'", d.Severity, d.OverloadID)
	if d.Signature != "" {
		fmt.Fprintf(&b, " resolved as '%s'", d.Signature)
	}
	if d.Ambiguous {
		b.WriteString(" (overload resolution is ambiguous: multiple candidate overloads recorded)")
	}
	if d.Location != nil {
		fmt.Fprintf(&b, " at line %d, column %d", d.Location.StartLine, d.Location.StartColumn)
	} else {
		b.WriteString(" at <unknown location>")
	}
	if d.Replacement != "" {
		fmt.Fprintf(&b, "; replacement: %s", d.Replacement)
	}
	return b.String()
}

// DeprecationChecker inspects type-checked Ast values for call sites which
// resolve to deprecated overloads.
//
// The checker operates on the compiler output: overloads are matched by the
// resolved overload IDs recorded during type-checking, not by function name
// substring matching.
type DeprecationChecker struct {
	rules []DeprecatedOverload
	byID  map[string]DeprecatedOverload
}

// NewDeprecationChecker creates a DeprecationChecker from an ordered set of
// deprecated overload rules.
func NewDeprecationChecker(rules ...DeprecatedOverload) (*DeprecationChecker, error) {
	if len(rules) == 0 {
		return nil, errors.New("deprecation checker requires at least one deprecated overload rule")
	}
	byID := make(map[string]DeprecatedOverload, len(rules))
	for _, r := range rules {
		if r.OverloadID == "" {
			return nil, errors.New("deprecation rule requires a non-empty overload id")
		}
		if _, found := byID[r.OverloadID]; found {
			return nil, fmt.Errorf("duplicate deprecation rule for overload id: %s", r.OverloadID)
		}
		byID[r.OverloadID] = r
	}
	return &DeprecationChecker{
		rules: append([]DeprecatedOverload{}, rules...),
		byID:  byID,
	}, nil
}

// Check returns the deprecation diagnostics for all call sites within a
// type-checked Ast which resolve to a configured deprecated overload.
//
// Diagnostics are deduplicated by rule and source position and returned in a
// stable order sorted by source location. Warnings and errors are both
// reported as data; neither prevents the caller from creating a Program from
// the Ast. The Env may be nil, in which case signatures are derived from the
// checked types recorded on the Ast rather than the environment declarations.
func (c *DeprecationChecker) Check(e *Env, a *Ast) ([]DeprecationDiagnostic, error) {
	if a == nil || !a.IsChecked() {
		return nil, errors.New("deprecation checks require a type-checked Ast")
	}
	impl := a.NativeRep()
	info := impl.SourceInfo()
	overloads := overloadIndex(e)

	var diags []DeprecationDiagnostic
	seen := make(map[string]bool)
	root := celast.NavigateAST(impl)
	calls := celast.MatchDescendants(root, celast.KindMatcher(celast.CallKind))
	for _, call := range calls {
		ref, found := impl.ReferenceMap()[call.ID()]
		if !found || len(ref.OverloadIDs) == 0 {
			continue
		}
		for _, overloadID := range ref.OverloadIDs {
			rule, deprecated := c.byID[overloadID]
			if !deprecated {
				continue
			}
			loc := resolveSourceRange(call, info)
			// Deduplicate hits for the same rule at the same source
			// position. When no source mapping exists, fall back to the
			// expression id so distinct call sites remain distinct.
			var key string
			if loc != nil {
				key = fmt.Sprintf("%s|%d:%d-%d:%d", overloadID,
					loc.StartLine, loc.StartColumn, loc.EndLine, loc.EndColumn)
			} else {
				key = fmt.Sprintf("%s|#%d", overloadID, call.ID())
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			diags = append(diags, DeprecationDiagnostic{
				OverloadID:   overloadID,
				FunctionName: call.AsCall().FunctionName(),
				Signature:    resolveSignature(overloads, impl, call, overloadID),
				Severity:     rule.Severity,
				Replacement:  rule.Replacement,
				Ambiguous:    len(ref.OverloadIDs) > 1,
				Location:     loc,
			})
		}
	}
	sort.SliceStable(diags, func(i, j int) bool {
		a, b := diags[i], diags[j]
		if (a.Location == nil) != (b.Location == nil) {
			return a.Location != nil
		}
		if a.Location != nil && b.Location != nil {
			if a.Location.StartLine != b.Location.StartLine {
				return a.Location.StartLine < b.Location.StartLine
			}
			if a.Location.StartColumn != b.Location.StartColumn {
				return a.Location.StartColumn < b.Location.StartColumn
			}
		}
		return a.OverloadID < b.OverloadID
	})
	return diags, nil
}

// overloadIndex builds a lookup from overload id to its declaring function
// name and declaration. A nil environment yields an empty index.
func overloadIndex(e *Env) map[string]*decls.OverloadDecl {
	index := make(map[string]*decls.OverloadDecl)
	if e == nil {
		return index
	}
	for _, fn := range e.Functions() {
		for _, o := range fn.OverloadDecls() {
			index[o.ID()] = o
		}
	}
	return index
}

// resolveSignature renders the signature of the resolved overload, preferring
// the environment declaration and falling back to the checked types recorded
// on the Ast when the declaration is unavailable.
func resolveSignature(overloads map[string]*decls.OverloadDecl, impl *celast.AST, call celast.NavigableExpr, overloadID string) string {
	if o, found := overloads[overloadID]; found {
		return formatOverloadSignature(call.AsCall().FunctionName(), o)
	}
	return formatCheckedSignature(impl, call)
}

// formatOverloadSignature renders a declared overload signature, using
// receiver-style formatting for member functions.
func formatOverloadSignature(name string, o *decls.OverloadDecl) string {
	args := o.ArgTypes()
	var b strings.Builder
	if o.IsMemberFunction() && len(args) > 0 {
		b.WriteString(FormatCELType(args[0]))
		b.WriteString(".")
		b.WriteString(name)
		args = args[1:]
	} else {
		b.WriteString(name)
	}
	b.WriteString("(")
	for i, arg := range args {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(FormatCELType(arg))
	}
	b.WriteString(") -> ")
	b.WriteString(FormatCELType(o.ResultType()))
	return b.String()
}

// formatCheckedSignature renders a call signature from the checked types
// recorded on the Ast.
func formatCheckedSignature(impl *celast.AST, nav celast.NavigableExpr) string {
	call := nav.AsCall()
	var b strings.Builder
	if call.IsMemberFunction() {
		b.WriteString(FormatCELType(impl.GetType(call.Target().ID())))
		b.WriteString(".")
	}
	b.WriteString(call.FunctionName())
	b.WriteString("(")
	for i, arg := range call.Args() {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(FormatCELType(impl.GetType(arg.ID())))
	}
	b.WriteString(") -> ")
	b.WriteString(FormatCELType(impl.GetType(nav.ID())))
	return b.String()
}

// resolveSourceRange computes the source range for a call expression.
//
// Macro-generated expressions carry the offset of the user-written macro
// call, so the direct offset lookup maps macro expansions back to the macro
// call site. When the call itself has no offset, ancestors are consulted
// since macro expansion roots record the user call site. If no source
// mapping exists at all, nil is returned rather than a fabricated position.
func resolveSourceRange(call celast.NavigableExpr, info *celast.SourceInfo) *SourceRange {
	e := call
	for {
		if rng, found := info.GetOffsetRange(e.ID()); found {
			start := info.GetLocationByOffset(rng.Start)
			stop := info.GetLocationByOffset(rng.Stop)
			return &SourceRange{
				StartLine:   start.Line(),
				StartColumn: start.Column(),
				EndLine:     stop.Line(),
				EndColumn:   stop.Column(),
			}
		}
		parent, found := e.Parent()
		if !found {
			return nil
		}
		e = parent
	}
}
