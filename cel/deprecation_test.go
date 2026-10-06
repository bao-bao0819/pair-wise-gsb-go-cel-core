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

package cel_test

import (
	"strings"
	"testing"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

func deprecationTestEnv(t *testing.T, opts ...cel.EnvOption) *cel.Env {
	t.Helper()
	base := []cel.EnvOption{
		cel.Variable("s", cel.StringType),
		cel.Function("convert",
			cel.Overload("convert_string", []*cel.Type{cel.StringType}, cel.IntType),
			cel.Overload("convert_int", []*cel.Type{cel.IntType}, cel.IntType),
			cel.SingletonFunctionBinding(func(args ...ref.Val) ref.Val {
				return types.Int(0)
			}),
		),
	}
	env, err := cel.NewEnv(append(base, opts...)...)
	if err != nil {
		t.Fatalf("cel.NewEnv() failed: %v", err)
	}
	return env
}

func TestDeprecatedOverloadWarningDoesNotBlockProgram(t *testing.T) {
	var warnings []*cel.DeprecatedOverloadFinding
	env := deprecationTestEnv(t,
		cel.DeprecatedOverloads(
			[]cel.DeprecatedOverload{{
				OverloadID:  "convert_string",
				Severity:    cel.DeprecatedOverloadWarning,
				Alternative: "convertInt()",
			}},
			func(findings []*cel.DeprecatedOverloadFinding) {
				warnings = append(warnings, findings...)
			},
		),
	)

	ast, iss := env.Compile("convert(s)")
	if iss != nil && iss.Err() != nil {
		t.Fatalf("Compile() returned an error for a warning: %v", iss.Err())
	}
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, wanted 1", len(warnings))
	}
	if warnings[0].Alternative != "convertInt()" {
		t.Errorf("alternative = %q, wanted convertInt()", warnings[0].Alternative)
	}

	// A warning must not prevent Program creation or evaluation.
	prg, err := env.Program(ast)
	if err != nil {
		t.Fatalf("env.Program() failed despite warning-only deprecation: %v", err)
	}
	out, _, err := prg.Eval(map[string]any{"s": "42"})
	if err != nil {
		t.Fatalf("prg.Eval() failed: %v", err)
	}
	if out.Value() != int64(0) {
		t.Logf("evaluated convert(\"42\") = %v (binding not required for this test)", out.Value())
	}
}

func TestDeprecatedOverloadErrorBlocksCompile(t *testing.T) {
	env := deprecationTestEnv(t,
		cel.DeprecatedOverloads(
			[]cel.DeprecatedOverload{{
				OverloadID:  "convert_string",
				Severity:    cel.DeprecatedOverloadError,
				Alternative: "convertInt()",
			}},
			nil,
		),
	)

	_, iss := env.Compile("convert(s)")
	if iss == nil || iss.Err() == nil {
		t.Fatal("Compile() succeeded, wanted a blocking deprecation error")
	}
	if !strings.Contains(iss.Err().Error(), "convert_string") {
		t.Errorf("error %q did not mention the deprecated overload id", iss.Err().Error())
	}

	// The non-deprecated sibling overload must still compile.
	if _, iss := env.Compile("convert(1)"); iss != nil && iss.Err() != nil {
		t.Errorf("current overload unexpectedly flagged: %v", iss.Err())
	}
}

func TestCheckDeprecatedOverloadsDirect(t *testing.T) {
	env := deprecationTestEnv(t)
	ast, iss := env.Compile("convert(s)")
	if iss != nil && iss.Err() != nil {
		t.Fatalf("Compile() failed: %v", iss.Err())
	}
	findings, err := env.CheckDeprecatedOverloads(ast, cel.DeprecatedOverload{
		OverloadID:  "convert_string",
		Severity:    cel.DeprecatedOverloadWarning,
		Alternative: "convertInt()",
	})
	if err != nil {
		t.Fatalf("CheckDeprecatedOverloads() failed: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("got %d findings, wanted 1", len(findings))
	}
	if findings[0].Function != "convert" || findings[0].OverloadID != "convert_string" {
		t.Errorf("unexpected finding %v", findings[0])
	}
}
