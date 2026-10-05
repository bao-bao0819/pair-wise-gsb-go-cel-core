package cel_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"

	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
	"google.golang.org/protobuf/proto"
)

const dependencyTestMessage = "test.DependencyMessage"

type dependencyTestProvider struct {
	fields map[string]*types.Type
}

func (p *dependencyTestProvider) EnumValue(string) ref.Val { return types.NewErr("not an enum") }

func (p *dependencyTestProvider) FindIdent(name string) (ref.Val, bool) {
	if name == dependencyTestMessage {
		return types.NewTypeTypeWithParam(types.NewObjectType(name)), true
	}
	return nil, false
}

func (p *dependencyTestProvider) FindStructType(name string) (*types.Type, bool) {
	if name != dependencyTestMessage {
		return nil, false
	}
	return types.NewTypeTypeWithParam(types.NewObjectType(name)), true
}

func (p *dependencyTestProvider) FindStructFieldNames(name string) ([]string, bool) {
	if name != dependencyTestMessage {
		return nil, false
	}
	names := make([]string, 0, len(p.fields))
	for name := range p.fields {
		names = append(names, name)
	}
	return names, true
}

func (p *dependencyTestProvider) FindStructFieldType(typeName, fieldName string) (*types.FieldType, bool) {
	if typeName != dependencyTestMessage {
		return nil, false
	}
	fieldType, found := p.fields[fieldName]
	if !found {
		return nil, false
	}
	return &types.FieldType{Type: fieldType}, true
}

func (p *dependencyTestProvider) NewValue(string, map[string]ref.Val) ref.Val {
	return types.NewErr("not constructible")
}

func TestDependencyContractCheckedASTCompatibility(t *testing.T) {
	baseEnv := newDependencyTestEnv(t, &dependencyTestProvider{fields: map[string]*types.Type{
		"used":   types.StringType,
		"unused": types.IntType,
	}},
		cel.Container("a.b"),
		cel.Variable("unusedTopLevel", cel.IntType),
		dependencyFunction(types.StringType, types.IntType),
	)
	checked, issues := baseEnv.Compile(`input.used.size() + dependencyFn(input.used) + size(extra)`)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("Compile() failed: %v", issues.Err())
	}

	checkedProto, err := cel.AstToCheckedExpr(checked)
	if err != nil {
		t.Fatalf("AstToCheckedExpr() failed: %v", err)
	}
	serializedAST, err := proto.Marshal(checkedProto)
	if err != nil {
		t.Fatalf("proto.Marshal(checked AST) failed: %v", err)
	}
	reloadedProto := &exprpb.CheckedExpr{}
	if err := proto.Unmarshal(serializedAST, reloadedProto); err != nil {
		t.Fatalf("proto.Unmarshal(checked AST) failed: %v", err)
	}
	reloadedAST, err := cel.CheckedExprToAstWithSource(reloadedProto, nil)
	if err != nil {
		t.Fatalf("CheckedExprToAstWithSource() failed: %v", err)
	}

	contract, err := cel.ExportDependencyContract(reloadedAST)
	if err != nil {
		t.Fatalf("ExportDependencyContract() failed: %v", err)
	}
	serializedContract, err := cel.MarshalDependencyContract(contract)
	if err != nil {
		t.Fatalf("MarshalDependencyContract() failed: %v", err)
	}
	if !strings.Contains(string(serializedContract), `"version":1`) {
		t.Fatalf("serialized contract missing version 1: %s", serializedContract)
	}
	serializedContractAgain, err := cel.MarshalDependencyContract(contract)
	if err != nil {
		t.Fatalf("MarshalDependencyContract() second call failed: %v", err)
	}
	if string(serializedContract) != string(serializedContractAgain) {
		t.Fatalf("contract serialization is not stable:\n%s\n%s", serializedContract, serializedContractAgain)
	}
	unsorted := *contract
	unsorted.Variables = append([]cel.VariableDependency(nil), contract.Variables...)
	unsorted.Fields = append([]cel.FieldDependency(nil), contract.Fields...)
	unsorted.Functions = append([]cel.FunctionDependency(nil), contract.Functions...)
	slices.Reverse(unsorted.Variables)
	slices.Reverse(unsorted.Fields)
	slices.Reverse(unsorted.Functions)
	serializedUnsorted, err := cel.MarshalDependencyContract(&unsorted)
	if err != nil {
		t.Fatalf("MarshalDependencyContract(unsorted) failed: %v", err)
	}
	if string(serializedUnsorted) != string(serializedContract) {
		t.Fatalf("unsorted contract did not serialize in stable order:\n%s\n%s", serializedUnsorted, serializedContract)
	}
	reloadedContract, err := cel.UnmarshalDependencyContract(serializedContract)
	if err != nil {
		t.Fatalf("UnmarshalDependencyContract() failed: %v", err)
	}
	if reloadedContract.ExpressionID != checked.Expr().GetId() {
		t.Fatalf("expression ID = %d, wanted %d", reloadedContract.ExpressionID, checked.Expr().GetId())
	}

	tests := []struct {
		name      string
		env       *cel.Env
		wantError string
	}{
		{
			name: "compatible despite unused changes",
			env: newDependencyTestEnv(t, &dependencyTestProvider{fields: map[string]*types.Type{
				"used":   types.StringType,
				"unused": types.StringType,
			}},
				cel.Container("a.b"),
				cel.Variable("unusedTopLevel", cel.StringType),
				dependencyFunction(types.StringType, types.IntType),
				cel.Function("a.b.dependencyFn",
					cel.Overload("dependency_fn_unused", []*cel.Type{cel.BoolType}, cel.BoolType,
						cel.FunctionBinding(func(values ...ref.Val) ref.Val { return values[0] }))),
			),
		},
		{
			name: "used field type changed",
			env: newDependencyTestEnv(t, &dependencyTestProvider{fields: map[string]*types.Type{
				"used":   types.IntType,
				"unused": types.IntType,
			}},
				cel.Container("a.b"),
				dependencyFunction(types.StringType, types.IntType),
			),
			wantError: "field input.used",
		},
		{
			name: "namespace resolves used function elsewhere",
			env: newDependencyTestEnv(t, &dependencyTestProvider{fields: map[string]*types.Type{
				"used":   types.StringType,
				"unused": types.IntType,
			}},
				cel.Container("x"),
				dependencyFunctionWithQualifiedName("x.dependencyFn", types.StringType, types.IntType),
			),
			wantError: "overload dependency_fn_string",
		},
		{
			name: "selected overload parameter changed",
			env: newDependencyTestEnv(t, &dependencyTestProvider{fields: map[string]*types.Type{
				"used":   types.StringType,
				"unused": types.IntType,
			}},
				cel.Container("a.b"),
				dependencyFunction(types.IntType, types.IntType),
			),
			wantError: "overload dependency_fn_string",
		},
		{
			name: "selected overload return changed",
			env: newDependencyTestEnv(t, &dependencyTestProvider{fields: map[string]*types.Type{
				"used":   types.StringType,
				"unused": types.IntType,
			}},
				cel.Container("a.b"),
				dependencyFunction(types.StringType, types.StringType),
			),
			wantError: "overload dependency_fn_string",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := reloadedContract.CheckCompatibility(test.env)
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("CheckCompatibility() failed: %v", err)
				}
				loaded, err := cel.LoadCheckedAST(reloadedAST, test.env)
				if err != nil {
					t.Fatalf("LoadCheckedAST() failed: %v", err)
				}
				if _, err := test.env.Program(loaded); err != nil {
					t.Fatalf("Program() failed after compatibility check: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("CheckCompatibility() got %v, wanted error containing %q", err, test.wantError)
			}
			incompatible := &cel.IncompatibleDependencyError{}
			if !errors.As(err, &incompatible) || incompatible.ExpressionID != checked.Expr().GetId() {
				t.Fatalf("CheckCompatibility() error = %T, wanted expression ID %d", err, checked.Expr().GetId())
			}
			if loaded, loadErr := cel.LoadCheckedAST(reloadedAST, test.env); loadErr == nil {
				if _, programErr := test.env.Program(loaded); programErr == nil {
					t.Fatalf("Program() unexpectedly succeeded after contract conflict")
				}
			}
		})
	}
}

func TestDependencyContractRejectsUncheckedAST(t *testing.T) {
	env := newDependencyTestEnv(t, &dependencyTestProvider{}, cel.Variable("input", cel.StringType))
	parsed, issues := env.Parse(`input`)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("Parse() failed: %v", issues.Err())
	}
	if _, err := cel.ExportDependencyContract(parsed); err == nil ||
		!strings.Contains(err.Error(), "unchecked ast") {
		t.Fatalf("ExportDependencyContract() got %v, wanted unchecked ast error", err)
	}
}

func TestDependencyContractDynamicAccessIsConservative(t *testing.T) {
	env, err := cel.NewEnv(cel.Variable("input", cel.DynType))
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	env, err = cel.NewEnv(
		cel.Variable("input", cel.DynType),
		cel.Variable("field", cel.StringType),
	)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	checked, issues := env.Compile(`input[field]`)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("Compile() failed: %v", issues.Err())
	}
	contract, err := cel.ExportDependencyContract(checked)
	if err != nil {
		t.Fatalf("ExportDependencyContract() failed: %v", err)
	}
	if len(contract.Dynamic) != 1 {
		t.Fatalf("dynamic dependencies = %d, wanted 1", len(contract.Dynamic))
	}
	if len(contract.Fields) != 0 {
		t.Fatalf("static fields = %d, wanted 0 for dynamic access", len(contract.Fields))
	}

	compatible, err := cel.NewEnv(
		cel.Variable("input", cel.DynType),
		cel.Variable("field", cel.StringType),
	)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	if err := contract.CheckCompatibility(compatible); err != nil {
		t.Fatalf("CheckCompatibility() rejected equal dynamic root: %v", err)
	}
	changed, err := cel.NewEnv(
		cel.Variable("input", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("field", cel.StringType),
	)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	if err := contract.CheckCompatibility(changed); err == nil ||
		!strings.Contains(err.Error(), "dynamic-access input") {
		t.Fatalf("CheckCompatibility() got %v, wanted conservative dynamic-access conflict", err)
	}
}

func TestDependencyContractDoesNotCompareImplementationIdentity(t *testing.T) {
	provider := &dependencyTestProvider{fields: map[string]*types.Type{"used": types.StringType}}
	base := newDependencyTestEnv(t, provider, cel.Container("a.b"),
		cel.Function("a.b.dependencyFn",
			cel.Overload("dependency_fn_string", []*cel.Type{cel.StringType}, cel.IntType,
				cel.UnaryBinding(func(ref.Val) ref.Val { return types.Int(1) }))))
	target := newDependencyTestEnv(t, provider, cel.Container("a.b"),
		cel.Function("a.b.dependencyFn",
			cel.Overload("dependency_fn_string", []*cel.Type{cel.StringType}, cel.IntType,
				cel.UnaryBinding(func(ref.Val) ref.Val { return types.Int(2) }))))
	checked, issues := base.Compile(`dependencyFn(input.used)`)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("Compile() failed: %v", issues.Err())
	}
	contract, err := cel.ExportDependencyContract(checked)
	if err != nil {
		t.Fatalf("ExportDependencyContract() failed: %v", err)
	}
	if err := contract.CheckCompatibility(target); err != nil {
		t.Fatalf("same signature with different implementation should remain compatible: %v", err)
	}
}

func newDependencyTestEnv(t *testing.T, provider *dependencyTestProvider, opts ...cel.EnvOption) *cel.Env {
	t.Helper()
	allOpts := append([]cel.EnvOption{
		cel.CustomTypeProvider(provider),
		cel.Variable("input", cel.ObjectType(dependencyTestMessage)),
		cel.Variable("extra", cel.StringType),
	}, opts...)
	env, err := cel.NewEnv(allOpts...)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	return env
}

func dependencyFunction(argType, resultType *types.Type) cel.EnvOption {
	return dependencyFunctionWithContainer("a.b", argType, resultType)
}

func dependencyFunctionWithContainer(container string, argType, resultType *types.Type) cel.EnvOption {
	return dependencyFunctionWithQualifiedName(container+".dependencyFn", argType, resultType)
}

func dependencyFunctionWithQualifiedName(name string, argType, resultType *types.Type) cel.EnvOption {
	id := "dependency_fn_" + cel.FormatCELType(argType)
	return cel.Function(name,
		cel.Overload(id, []*cel.Type{argType}, resultType,
			cel.UnaryBinding(func(ref.Val) ref.Val { return types.Int(1) })))
}
