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
	"fmt"

	"cel.dev/cel-go/checker/deprecation"
	"cel.dev/cel-go/common/ast"
)

// DeprecatedOverloadSeverity describes whether a deprecated overload use is a warning or an error.
type DeprecatedOverloadSeverity = deprecation.Severity

// Deprecated-overload severity levels.
const (
	// DeprecatedOverloadWarning surfaces a deprecated overload use as a non-blocking warning.
	DeprecatedOverloadWarning = deprecation.WarningSeverity

	// DeprecatedOverloadError treats a deprecated overload use as a blocking compile error.
	DeprecatedOverloadError = deprecation.ErrorSeverity
)

// DeprecatedOverload configures deprecation metadata for a single resolved overload id.
//
// Rules match the exact overload id recorded by the type checker, never a function-name
// substring, so a new overload sharing a function name with a deprecated one is unaffected.
type DeprecatedOverload struct {
	// OverloadID is the exact checked overload id to flag.
	OverloadID string

	// Severity determines whether a match is a warning or an error.
	Severity DeprecatedOverloadSeverity

	// Alternative is informational replacement guidance. It is returned on the finding and is
	// never applied to the user expression automatically.
	Alternative string
}

// DeprecatedOverloadFinding is a single deprecated overload use reported for an expression.
type DeprecatedOverloadFinding = deprecation.Finding

// DeprecatedOverloadWarningSink receives non-blocking warning findings after a successful check.
//
// Warnings do not prevent Ast or Program creation. The sink is invoked only for findings whose
// configured severity is warning; error findings are reported as compile issues instead.
type DeprecatedOverloadWarningSink func(findings []*DeprecatedOverloadFinding)

const deprecatedOverloadValidatorName = "cel.validator.deprecated_overloads"

// DeprecatedOverloads configures a post type-checking pass which flags calls resolving to the
// given deprecated overload ids.
//
// Error-severity matches are returned as compile issues and block Program creation, matching the
// behavior of other AST validators. Warning-severity matches do not block Ast or Program creation
// and are delivered to the optional sink.
//
// The validator matches the resolved overload id rather than the function name, so a new overload
// that happens to share a function name is never confused with a deprecated sibling.
func DeprecatedOverloads(overloads []DeprecatedOverload, sink DeprecatedOverloadWarningSink) EnvOption {
	rules := make([]deprecation.Rule, len(overloads))
	for i, o := range overloads {
		rules[i] = deprecation.Rule{
			OverloadID:  o.OverloadID,
			Severity:    o.Severity,
			Alternative: o.Alternative,
		}
	}
	return ASTValidators(&deprecatedOverloadValidator{rules: rules, sink: sink})
}

type deprecatedOverloadValidator struct {
	rules []deprecation.Rule
	sink  DeprecatedOverloadWarningSink
}

// Name returns the unique name of the validator.
func (v *deprecatedOverloadValidator) Name() string {
	return deprecatedOverloadValidatorName
}

// Validate scans the type-checked AST for deprecated overload uses, recording errors and
// collecting warnings.
func (v *deprecatedOverloadValidator) Validate(e *Env, _ ValidatorConfig, a *ast.AST, iss *Issues) {
	checker, err := deprecation.NewChecker(
		deprecation.Rules(v.rules...),
		deprecation.Functions(e.Functions()),
	)
	if err != nil {
		iss.ReportErrorAtID(0, "invalid deprecated overload configuration: %s", err.Error())
		return
	}
	findings := checker.Check(a)
	var warnings []*DeprecatedOverloadFinding
	for _, f := range findings {
		switch f.Severity {
		case deprecation.ErrorSeverity:
			iss.ReportErrorAtID(f.ExprID, "%s", f.Message())
		case deprecation.WarningSeverity:
			warnings = append(warnings, f)
		}
	}
	if v.sink != nil && len(warnings) > 0 {
		v.sink(warnings)
	}
}

// CheckDeprecatedOverloads runs a deprecation check directly against a type-checked Ast using the
// supplied rules, without requiring an environment-level validator.
//
// The resolved declaration signatures are left empty when they cannot be sourced from the
// environment; all other finding metadata is populated.
func (e *Env) CheckDeprecatedOverloads(a *Ast, overloads ...DeprecatedOverload) ([]*DeprecatedOverloadFinding, error) {
	rules := make([]deprecation.Rule, len(overloads))
	for i, o := range overloads {
		rules[i] = deprecation.Rule{
			OverloadID:  o.OverloadID,
			Severity:    o.Severity,
			Alternative: o.Alternative,
		}
	}
	checker, err := deprecation.NewChecker(
		deprecation.Rules(rules...),
		deprecation.Functions(e.Functions()),
	)
	if err != nil {
		return nil, fmt.Errorf("invalid deprecated overload configuration: %w", err)
	}
	return checker.Check(a.NativeRep()), nil
}
