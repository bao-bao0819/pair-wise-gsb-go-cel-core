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

// Package deprecation provides a post type-checking pass which reports call sites which resolve
// to function overloads that have been marked as deprecated.
//
// The checker operates on a type-checked ast.AST and matches calls by the exact overload ids
// recorded by the type checker during resolution; it never matches by function-name substring.
// Diagnostic results include the source line/column range, the set of actually resolved overload
// signatures, and an indication of resolution ambiguity when more than one overload matched.
package deprecation

import (
	"fmt"
	"slices"
	"strings"

	"cel.dev/cel-go/common"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/decls"
)

// Severity describes how a deprecated overload use should be treated.
type Severity int

const (
	// WarningSeverity indicates that a deprecated overload use should be surfaced as a
	// non-blocking warning. Warnings never prevent Program creation.
	WarningSeverity Severity = iota + 1

	// ErrorSeverity indicates that a deprecated overload use should be treated as a blocking
	// error.
	ErrorSeverity
)

// String returns the human-readable name of the severity.
func (s Severity) String() string {
	switch s {
	case WarningSeverity:
		return "warning"
	case ErrorSeverity:
		return "error"
	default:
		return "unknown"
	}
}

// Rule configures deprecation metadata for a single, fully-qualified overload id.
//
// The same function name may have several overloads, some deprecated and some not; rules are keyed
// strictly by overload id so that only the resolved, deprecated signature is reported.
type Rule struct {
	// OverloadID is the exact checked overload id to flag, e.g. "string_size".
	OverloadID string

	// Severity determines whether a match is reported as a warning or an error.
	Severity Severity

	// Alternative is free-form guidance returned to the caller describing a preferred replacement.
	// It is never applied to the user expression automatically.
	Alternative string
}

// SourceRange captures the 1-based line and 0-based column span of a call site.
type SourceRange struct {
	Start common.Location
	Stop  common.Location
}

// ResolvedSignature describes one overload the type checker associated with a call.
type ResolvedSignature struct {
	// OverloadID is the checked overload id.
	OverloadID string

	// Function is the fully-qualified resolved function name.
	Function string

	// Signature is a human rendering of the overload declaration signature. The value is empty
	// when the declaration set supplied to the checker does not contain the overload.
	Signature string
}

// Finding reports a single deprecated overload use at a specific call site.
type Finding struct {
	// ExprID is the id of the call expression node that resolved to the deprecated overload.
	ExprID int64

	// OverloadID is the configured deprecated overload id which matched.
	OverloadID string

	// Severity is the configured severity of the matched deprecation rule.
	Severity Severity

	// Function is the function name the call was resolved to by the type checker.
	Function string

	// Alternative is the configured replacement guidance. It is informational only.
	Alternative string

	// ResolvedOverloads enumerates the overloads the type checker associated with the call,
	// in declaration order. Only overloads that were actually selected as resolution candidates
	// are included; declarations that did not match are never listed.
	ResolvedOverloads []ResolvedSignature

	// Ambiguous indicates that more than one overload was a valid resolution candidate for the
	// call. In that case the runtime selects among the candidates, and the reported signature set
	// should be inspected to determine the actual behavior.
	Ambiguous bool

	// Range is the source range of the call. When FromMacroExpansion is true the range points at
	// the user-written macro invocation rather than the synthesized call inside the expansion.
	//
	// Range is nil when the AST carries no source information for the node; in that case
	// MissingSourceInfo is true and no line or column numbers are fabricated.
	Range *SourceRange

	// FromMacroExpansion indicates that the flagged call was synthesized by macro expansion and
	// has been attributed back to the enclosing user-written macro call.
	FromMacroExpansion bool

	// MissingSourceInfo indicates that no source location could be resolved. The finding remains
	// usable (it still carries the expression id and resolved signatures) but Range is nil.
	MissingSourceInfo bool
}

// Message renders a stable, human-readable description of the finding.
func (f *Finding) Message() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: call to deprecated overload %q", f.Severity, f.OverloadID)
	if f.Ambiguous {
		b.WriteString(" (overload resolution is ambiguous among the listed candidates)")
	}
	if f.Alternative != "" {
		fmt.Fprintf(&b, "; use %s instead", f.Alternative)
	}
	return b.String()
}

// Option configures a Checker.
type Option func(*Checker) error

// Rules configures the set of deprecation rules evaluated by the checker.
func Rules(rules ...Rule) Option {
	return func(c *Checker) error {
		c.rules = append(c.rules, rules...)
		return nil
	}
}

// Functions supplies the function declarations used to render resolved overload signatures.
//
// Typically this is the value returned by cel.Env.Functions(). When omitted, the checker still
// reports matches but leaves the declared signature empty.
func Functions(functions map[string]*decls.FunctionDecl) Option {
	return func(c *Checker) error {
		c.functions = functions
		return nil
	}
}

// Checker scans type-checked expressions for uses of deprecated overloads.
type Checker struct {
	rules     []Rule
	functions map[string]*decls.FunctionDecl
}

// NewChecker creates a deprecation Checker from the supplied options.
//
// An error is returned if a rule is malformed, e.g. has an empty overload id or an unknown
// severity.
func NewChecker(opts ...Option) (*Checker, error) {
	c := &Checker{}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}
	for _, r := range c.rules {
		if r.OverloadID == "" {
			return nil, fmt.Errorf("deprecation rule requires a non-empty overload id")
		}
		if r.Severity != WarningSeverity && r.Severity != ErrorSeverity {
			return nil, fmt.Errorf("deprecation rule for %q has invalid severity %d", r.OverloadID, r.Severity)
		}
	}
	return c, nil
}

// Check returns the set of deprecated-overload findings in the type-checked AST.
//
// The returned slice is deterministically ordered and de-duplicated: the same deprecation rule is
// reported at most once per call location. A nil AST yields no findings.
func (c *Checker) Check(a *ast.AST) []*Finding {
	if a == nil || a.Expr() == nil || len(c.rules) == 0 {
		return []*Finding{}
	}
	rulesByID := make(map[string]Rule, len(c.rules))
	for _, r := range c.rules {
		rulesByID[r.OverloadID] = r
	}

	signatures := c.indexOverloads()
	info := a.SourceInfo()
	// Macro anchors map zero-width synthesized call locations to the user-written macro span.
	macroAnchors := buildMacroAnchors(info)

	var findings []*Finding
	seen := make(map[string]struct{})
	ast.PreOrderVisit(a.Expr(), ast.NewExprVisitor(func(e ast.Expr) {
		if e.Kind() != ast.CallKind {
			return
		}
		ref, found := a.ReferenceMap()[e.ID()]
		if !found || len(ref.OverloadIDs) == 0 {
			return
		}
		call := e.AsCall()
		resolved := make([]ResolvedSignature, 0, len(ref.OverloadIDs))
		matched := make(map[string]struct{})
		for _, oid := range ref.OverloadIDs {
			sig := signatures[oid]
			sig.OverloadID = oid
			if sig.Function == "" {
				sig.Function = call.FunctionName()
			}
			resolved = append(resolved, sig)
			if _, deprecated := rulesByID[oid]; deprecated {
				matched[oid] = struct{}{}
			}
		}
		if len(matched) == 0 {
			return
		}

		rng, fromMacro, missing := resolveRange(info, e, macroAnchors)

		// Emit one finding per distinct deprecated rule that matched at this call.
		for _, oid := range ref.OverloadIDs {
			rule, ok := rulesByID[oid]
			if !ok {
				continue
			}
			dedupKey := fmt.Sprintf("%d|%s|%t|%t", e.ID(), rule.OverloadID, fromMacro, missing)
			if _, dup := seen[dedupKey]; dup {
				continue
			}
			seen[dedupKey] = struct{}{}
			findings = append(findings, &Finding{
				ExprID:             e.ID(),
				OverloadID:         rule.OverloadID,
				Severity:           rule.Severity,
				Function:           call.FunctionName(),
				Alternative:        rule.Alternative,
				ResolvedOverloads:  slices.Clone(resolved),
				Ambiguous:          len(ref.OverloadIDs) > 1,
				Range:              rng,
				FromMacroExpansion: fromMacro,
				MissingSourceInfo:  missing,
			})
		}
	}))

	sortFindings(findings)
	return findings
}

// indexOverloads maps every declared overload id to a renderable signature.
func (c *Checker) indexOverloads() map[string]ResolvedSignature {
	signatures := map[string]ResolvedSignature{}
	for fnName, fn := range c.functions {
		for _, overload := range fn.OverloadDecls() {
			signatures[overload.ID()] = ResolvedSignature{
				OverloadID: overload.ID(),
				Function:   fnName,
				Signature:  formatSignature(fnName, overload),
			}
		}
	}
	return signatures
}

// macroAnchor records a user-written macro call span. zeroStart is the zero-width offset at which
// synthesized calls are pinned; rng is the reconstructed full span of the user code.
type macroAnchor struct {
	exprID int64
	rng    ast.OffsetRange
}

// Anchors are keyed by the zero-width offset at which synthesized calls are pinned.
func buildMacroAnchors(info *ast.SourceInfo) map[int32]macroAnchor {
	anchors := map[int32]macroAnchor{}
	if info == nil {
		return anchors
	}
	for id := range info.MacroCalls() {
		anchorRange, found := info.GetOffsetRange(id)
		if !found {
			continue
		}
		zeroStart := anchorRange.Start
		// The anchor node itself is zero-width (pinned to the macro's opening parenthesis), so the
		// full user-written span is reconstructed from the original call signature captured for
		// the macro. Nested macro arguments are stored as unspecified placeholders whose id points
		// at the nested macro anchor, allowing recursive span computation.
		call, isCall := info.GetMacroCall(id)
		if isCall {
			if span, ok := macroCallSpan(info, call); ok {
				anchorRange = span
			}
		}
		anchors[zeroStart] = macroAnchor{exprID: id, rng: anchorRange}
	}
	return anchors
}

// macroCallSpan derives the smallest source range covering the user-written macro call, using
// the recorded original signature (target and arguments).
func macroCallSpan(info *ast.SourceInfo, call ast.Expr) (ast.OffsetRange, bool) {
	min, max := int32(-1), int32(-1)
	extend := func(r ast.OffsetRange) {
		if min == -1 || r.Start < min {
			min = r.Start
		}
		if r.Stop > max {
			max = r.Stop
		}
	}
	var visit func(e ast.Expr)
	visit = func(e ast.Expr) {
		if r, found := info.GetOffsetRange(e.ID()); found && r.Stop > r.Start {
			extend(r)
			return
		}
		// Zero-width or missing nodes may themselves be macro anchors: descend into their recorded
		// original call signature so nested macro invocations are covered.
		if nested, isMacro := info.GetMacroCall(e.ID()); isMacro {
			visit(nested)
			return
		}
		switch e.Kind() {
		case ast.CallKind:
			c := e.AsCall()
			if c.IsMemberFunction() {
				visit(c.Target())
			}
			for _, arg := range c.Args() {
				visit(arg)
			}
		case ast.ListKind:
			for _, elem := range e.AsList().Elements() {
				visit(elem)
			}
		case ast.SelectKind:
			visit(e.AsSelect().Operand())
		}
	}
	visit(call)
	if min == -1 {
		return ast.OffsetRange{}, false
	}
	return ast.OffsetRange{Start: min, Stop: max}, true
}

// resolveRange determines the user-facing source range of a call, attributing synthesized macro
// calls back to the enclosing macro invocation. It never invents line numbers: when no range is
// available the missing flag is set and the returned range is nil.
func resolveRange(info *ast.SourceInfo, e ast.Expr, anchors map[int32]macroAnchor) (*SourceRange, bool, bool) {
	if info == nil {
		return nil, false, true
	}
	r, found := info.GetOffsetRange(e.ID())
	if !found {
		return nil, false, true
	}
	// Synthesized macro nodes are created with a zero-width location pinned to the start of the
	// user-written macro call. Attribute the finding to the macro's full span.
	if r.Start == r.Stop {
		if anchor, ok := anchors[r.Start]; ok {
			return anchorRange(info, anchor), true, false
		}
	}
	return &SourceRange{
		Start: info.GetLocationByOffset(r.Start),
		Stop:  info.GetLocationByOffset(r.Stop),
	}, false, false
}

func anchorRange(info *ast.SourceInfo, anchor macroAnchor) *SourceRange {
	stop := anchor.rng.Stop
	if stop <= anchor.rng.Start {
		// Fall back to the zero-width anchor location when no wider span could be reconstructed
		// (e.g. a macro with only synthesized arguments).
		stop = anchor.rng.Start
	}
	return &SourceRange{
		Start: info.GetLocationByOffset(anchor.rng.Start),
		Stop:  info.GetLocationByOffset(stop),
	}
}
func sortFindings(findings []*Finding) {
	slices.SortStableFunc(findings, func(a, b *Finding) int {
		if a.ExprID != b.ExprID {
			return cmpInt64(a.ExprID, b.ExprID)
		}
		if a.OverloadID != b.OverloadID {
			return strings.Compare(a.OverloadID, b.OverloadID)
		}
		return strings.Compare(a.Severity.String(), b.Severity.String())
	})
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// formatSignature renders an overload declaration in a receiver- or global-call style.
func formatSignature(fnName string, o *decls.OverloadDecl) string {
	args := make([]string, len(o.ArgTypes()))
	for i, at := range o.ArgTypes() {
		args[i] = at.String()
	}
	ret := o.ResultType().String()
	if o.IsMemberFunction() && len(args) > 0 {
		target := args[0]
		return fmt.Sprintf("%s.%s(%s) -> %s", target, fnName, strings.Join(args[1:], ", "), ret)
	}
	return fmt.Sprintf("%s(%s) -> %s", fnName, strings.Join(args, ", "), ret)
}
