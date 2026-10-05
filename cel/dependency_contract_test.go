package cel

import (
	"strings"
	"testing"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/test/proto3pb"
)

const dependencyContractTestMessage = "google.expr.proto3.test.TestAllTypes"

type dependencyContractProvider struct {
	delegate      types.Provider
	overrideField string
	overrideType  *types.Type
}

func (p *dependencyContractProvider) EnumValue(enumName string) ref.Val {
	return p.delegate.EnumValue(enumName)
}

func (p *dependencyContractProvider) FindIdent(identName string) (ref.Val, bool) {
	return p.delegate.FindIdent(identName)
}

func (p *dependencyContractProvider) FindStructType(structType string) (*types.Type, bool) {
	return p.delegate.FindStructType(structType)
}

func (p *dependencyContractProvider) FindStructFieldNames(structType string) ([]string, bool) {
	return p.delegate.FindStructFieldNames(structType)
}

func (p *dependencyContractProvider) FindStructFieldType(structType, fieldName string) (*types.FieldType, bool) {
	fieldType, found := p.delegate.FindStructFieldType(structType, fieldName)
	if found && structType == dependencyContractTestMessage && fieldName == p.overrideField {
		clone := *fieldType
		clone.Type = p.overrideType
		fieldType = &clone
	}
	return fieldType, found
}

func (p *dependencyContractProvider) NewValue(structType string, fields map[string]ref.Val) ref.Val {
	return p.delegate.NewValue(structType, fields)
}

func newDependencyContractEnv(t *testing.T, container, fieldType, overloadArgType string, overrideField bool) *Env {
	t.Helper()
	registry, err := types.NewRegistry(&proto3pb.TestAllTypes{})
	if err != nil {
		t.Fatalf("types.NewRegistry() failed: %v", err)
	}
	provider := types.Provider(registry)
	if overrideField {
		provider = &dependencyContractProvider{delegate: registry, overrideField: "single_int64", overrideType: types.StringType}
	}
	objectType := types.NewObjectType(dependencyContractTestMessage)
	if fieldType == "dyn" {
		objectType = DynType
	}
	opts := []EnvOption{
		CustomTypeProvider(provider),
		Variable("obj", objectType),
		Variable("unused", types.IntType),
	}
	describeArgType := IntType
	describeBinding := types.String("int")
	if overloadArgType == "uint" {
		describeArgType = UintType
		describeBinding = types.String("changed")
	}
	opts = append(opts, Function("describe",
		Overload("describe_int", []*Type{describeArgType}, StringType, UnaryBinding(func(value ref.Val) ref.Val {
			return describeBinding
		})),
		Overload("describe_string", []*Type{StringType}, StringType, UnaryBinding(func(value ref.Val) ref.Val {
			return types.String("string")
		})),
	))
	if container != "" {
		opts = append(opts, Container(container))
	}
	env, err := NewEnv(opts...)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	return env
}

func loadSerializedCheckedASTWithContract(t *testing.T, a *Ast, contract *DependencyContract) *Ast {
	t.Helper()
	checkedExpr, err := AstToCheckedExpr(a)
	if err != nil {
		t.Fatalf("AstToCheckedExpr() failed: %v", err)
	}
	contractBytes, err := MarshalDependencyContract(contract)
	if err != nil {
		t.Fatalf("MarshalDependencyContract() failed: %v", err)
	}
	loadedContract, err := UnmarshalDependencyContract(contractBytes)
	if err != nil {
		t.Fatalf("UnmarshalDependencyContract() failed: %v", err)
	}
	loadedAst, err := CheckedExprToAstWithDependencyContract(checkedExpr, loadedContract, a.Source())
	if err != nil {
		t.Fatalf("CheckedExprToAstWithDependencyContract() failed: %v", err)
	}
	return loadedAst
}

func TestDependencyContractCompatibility(t *testing.T) {
	source := newDependencyContractEnv(t, "cache.worker", "int", "int", false)
	checked, issues := source.Compile("describe(obj.single_int64)")
	if issues.Err() != nil {
		t.Fatalf("Compile() failed: %v", issues.Err())
	}
	contract, err := source.ExportDependencyContract(checked)
	if err != nil {
		t.Fatalf("ExportDependencyContract() failed: %v", err)
	}
	contractBytes, err := MarshalDependencyContract(contract)
	if err != nil {
		t.Fatalf("MarshalDependencyContract() failed: %v", err)
	}
	if !strings.Contains(string(contractBytes), `"version":1`) {
		t.Fatalf("MarshalDependencyContract() = %s, missing version", contractBytes)
	}
	loadedContract, err := UnmarshalDependencyContract(contractBytes)
	if err != nil {
		t.Fatalf("UnmarshalDependencyContract() failed: %v", err)
	}
	stableBytes, err := MarshalDependencyContract(loadedContract)
	if err != nil {
		t.Fatalf("MarshalDependencyContract(loaded) failed: %v", err)
	}
	if string(stableBytes) != string(contractBytes) {
		t.Fatalf("dependency contract serialization is not stable: first=%s second=%s", contractBytes, stableBytes)
	}
	reloaded := loadSerializedCheckedASTWithContract(t, checked, loadedContract)

	t.Run("compatible unrelated variables and namespaces", func(t *testing.T) {
		target := newDependencyContractEnv(t, "cache.worker", "int", "int", false)
		target, err = target.Extend(Variable("added", StringType))
		if err != nil {
			t.Fatalf("target.Extend() failed: %v", err)
		}
		program, err := target.Program(reloaded)
		if err != nil {
			t.Fatalf("Program() returned compatibility error: %v", err)
		}
		if program == nil {
			t.Fatal("Program() returned nil program")
		}
	})

	t.Run("used field changed", func(t *testing.T) {
		target := newDependencyContractEnv(t, "cache.worker", "int", "int", true)
		_, err = target.Program(reloaded)
		mismatches := AsDependencyMismatches(err)
		if len(mismatches) != 1 || mismatches[0].Kind != "field" || mismatches[0].ExprID == 0 || mismatches[0].Path != dependencyContractTestMessage+".single_int64" {
			t.Fatalf("Program() error = %v, mismatches = %v, want one field mismatch with expression id and path", err, mismatches)
		}
	})

	t.Run("unrelated field change remains compatible", func(t *testing.T) {
		targetRegistry, err := types.NewRegistry(&proto3pb.TestAllTypes{})
		if err != nil {
			t.Fatalf("types.NewRegistry() failed: %v", err)
		}
		target := newDependencyContractEnv(t, "cache.worker", "int", "int", false)
		target.provider = &dependencyContractProvider{delegate: targetRegistry, overrideField: "single_string", overrideType: types.BoolType}
		program, err := target.Program(reloaded)
		if err != nil {
			t.Fatalf("Program() returned compatibility error for unused field: %v", err)
		}
		if program == nil {
			t.Fatal("Program() returned nil program")
		}
	})

	t.Run("namespace changed", func(t *testing.T) {
		target := newDependencyContractEnv(t, "other.namespace", "int", "int", false)
		_, err = target.Program(reloaded)
		mismatches := AsDependencyMismatches(err)
		if len(mismatches) != 1 || mismatches[0].Kind != "namespace" {
			t.Fatalf("Program() error = %v, mismatches = %v, want namespace mismatch", err, mismatches)
		}
	})

	t.Run("selected overload signature changed", func(t *testing.T) {
		target := newDependencyContractEnv(t, "cache.worker", "int", "uint", false)
		_, err = target.Program(reloaded)
		mismatches := AsDependencyMismatches(err)
		if len(mismatches) == 0 || mismatches[0].Kind != "overload" || mismatches[0].ExprID == 0 || !strings.Contains(mismatches[0].Path, "describe#describe_int") {
			t.Fatalf("Program() error = %v, mismatches = %v, want overload mismatch with expression id", err, mismatches)
		}
	})
}

func TestDependencyContractRequiresCheckedAST(t *testing.T) {
	env := newDependencyContractEnv(t, "", "int", "int", false)
	parsed, issues := env.Parse("describe(obj.single_int64)")
	if issues.Err() != nil {
		t.Fatalf("Parse() failed: %v", issues.Err())
	}
	if _, err := ExportDependencyContract(parsed); err == nil {
		t.Fatal("ExportDependencyContract(parsed) succeeded, want error")
	}
	checked, issues := env.Compile("true")
	if issues.Err() != nil {
		t.Fatalf("Compile() failed: %v", issues.Err())
	}
	contract, err := env.ExportDependencyContract(checked)
	if err != nil {
		t.Fatalf("ExportDependencyContract() failed: %v", err)
	}
	attached, err := WithDependencyContract(parsed, contract)
	if err == nil {
		t.Fatalf("WithDependencyContract() = %v, want unchecked ast error", attached)
	}
	other, issues := env.Compile("1 + 2")
	if issues.Err() != nil {
		t.Fatalf("Compile(false) failed: %v", issues.Err())
	}
	otherContract, err := env.ExportDependencyContract(other)
	if err != nil {
		t.Fatalf("ExportDependencyContract(false) failed: %v", err)
	}
	attached, err = WithDependencyContract(parsed, otherContract)
	if err == nil {
		t.Fatal("WithDependencyContract(parsed, other contract) succeeded, want unchecked ast error")
	}
	mismatched, err := WithDependencyContract(checked, otherContract)
	if err == nil {
		t.Fatalf("WithDependencyContract(checked, other contract) = %v, want expression id mismatch", mismatched)
	}
}
func TestDependencyContractDynamicAccessIsConservative(t *testing.T) {
	env, err := NewEnv(
		Variable("obj", DynType),
		Variable("field_name", StringType),
		Variable("unused", IntType),
	)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	checked, issues := env.Compile("obj[field_name]")
	if issues.Err() != nil {
		t.Fatalf("Compile() failed: %v", issues.Err())
	}
	contract, err := env.ExportDependencyContract(checked)
	if err != nil {
		t.Fatalf("ExportDependencyContract() failed: %v", err)
	}
	varNames := map[string]bool{}
	for _, variable := range contract.Variables {
		varNames[variable.Name] = true
	}
	if !varNames["obj"] || !varNames["field_name"] || varNames["unused"] {
		t.Fatalf("contract variables = %v, want obj and field_name without unused", contract.Variables)
	}
	if len(contract.Fields) != 0 || len(contract.DynamicTypes) != 0 {
		t.Fatalf("dynamic access produced static dependencies fields=%v dynamic_types=%v", contract.Fields, contract.DynamicTypes)
	}
	objVariable := contract.Variables[0]
	for _, variable := range contract.Variables {
		if variable.Name == "obj" {
			objVariable = variable
		}
	}
	if !objVariable.Dynamic {
		t.Fatalf("contract variables = %v, want conservative dynamic marker", contract.Variables)
	}
	reloaded := loadSerializedCheckedASTWithContract(t, checked, contract)
	target := newDependencyContractEnv(t, "", "int", "int", false)
	if _, err := target.Program(reloaded); err == nil {
		t.Fatal("Program() with concrete message target succeeded, want conservative dynamic dependency mismatch")
	}
}
