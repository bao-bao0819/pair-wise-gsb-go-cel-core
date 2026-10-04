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

package checker

import (
	"testing"

	"cel.dev/cel-go/common"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/containers"
	"cel.dev/cel-go/common/decls"
	"cel.dev/cel-go/common/stdlib"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/parser"
)

func scratchEnv(t *testing.T) *Env {
	t.Helper()
	reg := types.NewEmptyRegistry()
	env, err := NewEnv(containers.DefaultContainer, reg)
	if err != nil {
		t.Fatal(err)
	}
	vars := []*decls.VariableDecl{
		decls.NewVariable("items", types.NewListType(types.StringType)),
		decls.NewVariable("x", types.StringType),
	}
	if err := env.AddIdents(vars...); err != nil {
		t.Fatal(err)
	}
	if err := env.AddFunctions(stdlib.Functions()...); err != nil {
		t.Fatal(err)
	}
	return env
}

func scratchCheck(t *testing.T, env *Env, text string) (*ast.AST, common.Source) {
	t.Helper()
	src := common.NewStringSource(text, text)
	p, err := parser.NewParser(parser.Macros(parser.AllMacros...), parser.PopulateMacroCalls(true))
	if err != nil {
		t.Fatal(err)
	}
	pe, errs := p.Parse(src)
	if len(errs.GetErrors()) != 0 {
		t.Fatalf("parse errors: %s", errs.ToDisplayString())
	}
	checked, errs := Check(pe, src, env)
	if len(errs.GetErrors()) != 0 {
		t.Fatalf("check errors: %s", errs.ToDisplayString())
	}
	return checked, src
}
