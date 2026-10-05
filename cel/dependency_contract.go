package cel

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"cel.dev/cel-go/checker"
	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/decls"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/types"
)

const dependencyContractFormatVersion = 1

// DependencyContract records the checked AST's precise environment requirements.
//
// Unused declarations and overloads are intentionally omitted. Equal overload signatures
// establish static type compatibility only; they do not assert that host implementations behave
// the same way.
type DependencyContract struct {
	Version      int                       `json:"version"`
	ExpressionID int64                     `json:"expressionId"`
	Variables    []VariableDependency      `json:"variables"`
	Fields       []FieldDependency         `json:"fields"`
	Functions    []FunctionDependency      `json:"functions"`
	Dynamic      []DynamicAccessDependency `json:"dynamicAccesses"`

	localVars map[string]struct{}
}

// VariableDependency describes one global variable referenced by the checked expression.
type VariableDependency struct {
	ExprID int64   `json:"exprId"`
	Name   string  `json:"name"`
	Type   TypeRef `json:"type"`
}

// FieldDependency describes one statically selected message or struct field.
type FieldDependency struct {
	ExprID   int64         `json:"exprId"`
	Variable string        `json:"variable,omitempty"`
	TypeName string        `json:"typeName,omitempty"`
	Path     []PathSegment `json:"path,omitempty"`
	Field    string        `json:"field"`
	Type     TypeRef       `json:"type"`
}

// FunctionDependency describes one selected function call and its overload candidates.
type FunctionDependency struct {
	ExprID    int64              `json:"exprId"`
	Name      string             `json:"name"`
	Overloads []OverloadContract `json:"overloads"`
}

// OverloadContract describes one selected overload's static signature.
type OverloadContract struct {
	ID            string    `json:"id"`
	Member        bool      `json:"member"`
	ArgumentTypes []TypeRef `json:"argumentTypes"`
	ResultType    TypeRef   `json:"resultType"`
}

// DynamicAccessDependency describes an access which may reach fields only at runtime.
type DynamicAccessDependency struct {
	ExprID    int64         `json:"exprId"`
	Variable  string        `json:"variable"`
	Path      []PathSegment `json:"path"`
	Operation string        `json:"operation"`
	Type      TypeRef       `json:"type"`
}

// PathSegment is one static or dynamic qualifier in an access path.
type PathSegment struct {
	Field   string `json:"field,omitempty"`
	Index   string `json:"index,omitempty"`
	Dynamic bool   `json:"dynamic,omitempty"`
}

// TypeRef is a portable, host-pointer-free representation of a checked CEL type.
type TypeRef struct {
	Kind       uint      `json:"kind"`
	Name       string    `json:"name,omitempty"`
	Parameters []TypeRef `json:"parameters,omitempty"`
	Nullable   bool      `json:"nullable,omitempty"`
}

// DependencyMismatch identifies one contract-to-environment difference.
type DependencyMismatch struct {
	ExprID   int64
	Kind     string
	Symbol   string
	Expected string
	Actual   string
}

// IncompatibleDependencyError reports contract differences before program planning.
type IncompatibleDependencyError struct {
	ExpressionID int64
	Mismatches   []DependencyMismatch
}

func (e *IncompatibleDependencyError) Error() string {
	lines := []string{fmt.Sprintf("expression %d is incompatible with the target environment", e.ExpressionID)}
	for _, mismatch := range e.Mismatches {
		lines = append(lines, fmt.Sprintf("  expr %d: %s %s: expected %s, got %s",
			mismatch.ExprID, mismatch.Kind, mismatch.Symbol, mismatch.Expected, mismatch.Actual))
	}
	return strings.Join(lines, "\n")
}

func ExportDependencyContract(a *Ast) (*DependencyContract, error) {
	if a == nil || a.NativeRep() == nil {
		return nil, fmt.Errorf("cannot export dependency contract from nil ast")
	}
	if !a.IsChecked() {
		return nil, fmt.Errorf("cannot export dependency contract from unchecked ast")
	}
	native := a.NativeRep()
	contract := &DependencyContract{
		Version:      dependencyContractFormatVersion,
		ExpressionID: native.Expr().ID(),
		localVars:    collectLocalVariables(native),
	}
	celast.PostOrderVisit(native.Expr(), dependencyVisitor{ast: native, contract: contract})
	sortContract(contract)
	return contract, nil
}

// MarshalDependencyContract returns deterministic JSON containing the contract format version.
func MarshalDependencyContract(contract *DependencyContract) ([]byte, error) {
	if contract == nil {
		return nil, fmt.Errorf("cannot marshal nil dependency contract")
	}
	if contract.Version != dependencyContractFormatVersion {
		return nil, fmt.Errorf("unsupported dependency contract version: %d", contract.Version)
	}
	sortContract(contract)
	contract.localVars = nil
	return json.Marshal(contract)
}

// UnmarshalDependencyContract parses and validates the contract format version.
func UnmarshalDependencyContract(data []byte) (*DependencyContract, error) {
	var contract DependencyContract
	if err := json.Unmarshal(data, &contract); err != nil {
		return nil, err
	}
	if contract.Version != dependencyContractFormatVersion {
		return nil, fmt.Errorf("unsupported dependency contract version: %d", contract.Version)
	}
	sortContract(&contract)
	return &contract, nil
}

// CheckCompatibility compares only referenced dependencies with the target environment.
func (c *DependencyContract) CheckCompatibility(env *Env) error {
	if c == nil {
		return fmt.Errorf("cannot check nil dependency contract")
	}
	if c.Version != dependencyContractFormatVersion {
		return fmt.Errorf("unsupported dependency contract version: %d", c.Version)
	}
	if env == nil {
		return fmt.Errorf("cannot check dependency contract against nil environment")
	}

	var mismatches []DependencyMismatch
	dynamicExprs := map[int64]bool{}
	for _, dynamic := range c.Dynamic {
		dynamicExprs[dynamic.ExprID] = true
	}
	for _, variable := range c.Variables {
		actual := env.resolveVariableType(variable.Name)
		if actual == nil || !sameTypeRef(variable.Type, typeRef(actual)) {
			mismatches = append(mismatches, makeMismatch(variable.ExprID, "variable", variable.Name, variable.Type, actual))
		}
	}
	for _, field := range c.Fields {
		actual := env.resolveFieldType(field)
		if actual == nil || !sameTypeRef(field.Type, typeRef(actual)) {
			symbol := formatPathSegments(field.Variable, append(slices.Clone(field.Path), PathSegment{Field: field.Field}))
			if field.TypeName != "" {
				symbol = field.TypeName + "." + field.Field
			}
			mismatches = append(mismatches, makeMismatch(field.ExprID, "field", symbol, field.Type, actual))
		}
	}
	for _, call := range c.Functions {
		fn := env.resolveFunction(call.Name)
		if fn == nil {
			for _, overload := range call.Overloads {
				mismatches = append(mismatches, missingOverloadMismatch(call.ExprID, overload))
			}
			continue
		}
		declarations := map[string]*decls.OverloadDecl{}
		for _, overload := range fn.OverloadDecls() {
			declarations[overload.ID()] = overload
		}
		for _, overload := range call.Overloads {
			declared := declarations[overload.ID]
			if declared == nil {
				mismatches = append(mismatches, missingOverloadMismatch(call.ExprID, overload))
			} else if !dynamicExprs[call.ExprID] &&
				(declared.IsMemberFunction() != overload.Member || !overloadMatches(declared, overload)) {
				mismatches = append(mismatches, overloadMismatch(call.ExprID, overload, declared))
			}
		}
	}
	for _, dynamic := range c.Dynamic {
		actual := env.resolveDynamicType(dynamic)
		if actual == nil || !sameTypeRef(dynamic.Type, typeRef(actual)) {
			mismatches = append(mismatches, makeMismatch(dynamic.ExprID, "dynamic-access",
				formatPathSegments(dynamic.Variable, dynamic.Path), dynamic.Type, actual))
		}
	}
	if len(mismatches) != 0 {
		return &IncompatibleDependencyError{ExpressionID: c.ExpressionID, Mismatches: mismatches}
	}
	return nil
}

// LoadCheckedAST checks a checked AST contract before allowing it to be planned in the env.
func LoadCheckedAST(a *Ast, env *Env) (*Ast, error) {
	contract, err := ExportDependencyContract(a)
	if err != nil {
		return nil, err
	}
	if err := contract.CheckCompatibility(env); err != nil {
		return nil, err
	}
	return a, nil
}

type dependencyVisitor struct {
	ast      *celast.AST
	contract *DependencyContract
}

func (v dependencyVisitor) VisitExpr(e celast.Expr) {
	switch e.Kind() {
	case celast.IdentKind:
		v.contract.addVariable(v.ast, e.ID(), e.AsIdent())
	case celast.SelectKind:
		v.contract.addSelectDependency(v.ast, e.ID(), e.AsSelect())
	case celast.CallKind:
		v.contract.addCallDependency(v.ast, e.ID(), e.AsCall())
	case celast.StructKind:
		structExpr := e.AsStruct()
		for _, entry := range structExpr.Fields() {
			field := entry.AsStructField()
			v.contract.Fields = append(v.contract.Fields, FieldDependency{
				ExprID:   entry.ID(),
				TypeName: normalizeSymbol(structExpr.TypeName()),
				Field:    field.Name(),
				Type:     typeRef(v.ast.GetType(field.Value().ID())),
			})
		}
	}
}

func (v dependencyVisitor) VisitEntryExpr(celast.EntryExpr) {}

func (c *DependencyContract) addVariable(a *celast.AST, id int64, name string) {
	name = normalizeSymbol(name)
	if _, local := c.localVars[name]; local {
		return
	}
	t := a.GetType(id)
	if t.Kind() == types.TypeKind {
		return
	}
	c.Variables = append(c.Variables, VariableDependency{ExprID: id, Name: name, Type: typeRef(t)})
}

func (c *DependencyContract) addSelectDependency(a *celast.AST, id int64, sel celast.SelectExpr) {
	root, path, dynamicBefore, found := expressionPath(sel.Operand())
	if !found {
		return
	}
	target := unwrapOptional(a.GetType(sel.Operand().ID()))
	if target.Kind() == types.StructKind && !dynamicBefore {
		c.Fields = append(c.Fields, FieldDependency{
			ExprID: id, Variable: root, Path: path, Field: sel.FieldName(), Type: typeRef(a.GetType(id)),
		})
		return
	}
	if target.Kind() == types.StructKind || target.Kind() == types.DynKind || target.Kind() == types.AnyKind {
		c.addDynamic(id, root, append(path, PathSegment{Field: sel.FieldName()}), "select", a.GetType(id))
	}
}

func (c *DependencyContract) addCallDependency(a *celast.AST, id int64, call celast.CallExpr) {
	if call == nil {
		return
	}
	if call.FunctionName() == operators.OptSelect {
		if len(call.Args()) == 2 {
			if field, ok := literalString(call.Args()[1]); ok {
				c.addOptSelectDependency(a, id, call.Args()[0], field)
			}
		}
		return
	}

	if reference, found := a.ReferenceMap()[id]; found && len(reference.OverloadIDs) != 0 {
		args := []TypeRef{}
		if call.IsMemberFunction() && call.Target() != nil {
			args = append(args, typeRef(a.GetType(call.Target().ID())))
		}
		for _, arg := range call.Args() {
			args = append(args, typeRef(a.GetType(arg.ID())))
		}
		overloads := make([]OverloadContract, 0, len(reference.OverloadIDs))
		for _, overloadID := range reference.OverloadIDs {
			overloads = append(overloads, OverloadContract{
				ID: overloadID, Member: call.IsMemberFunction(), ArgumentTypes: args, ResultType: typeRef(a.GetType(id)),
			})
		}
		c.Functions = append(c.Functions, FunctionDependency{
			ExprID: id, Name: normalizeSymbol(call.FunctionName()), Overloads: overloads,
		})
	}

	if call.IsMemberFunction() {
		target := unwrapOptional(a.GetType(call.Target().ID()))
		if call.FunctionName() == operators.Index || call.FunctionName() == operators.OptIndex {
			if root, path, _, found := expressionPath(call.Target()); found {
				segment := PathSegment{Dynamic: true}
				if index, ok := literalIndex(call.Args()[0]); ok {
					segment.Index = index
				}
				c.addDynamic(id, root, append(path, segment), "index", a.GetType(id))
			}
		} else if target.Kind() == types.DynKind || target.Kind() == types.AnyKind {
			if root, path, _, found := expressionPath(call.Target()); found {
				c.addDynamic(id, root, path, "call:"+call.FunctionName(), a.GetType(id))
			}
		}
	} else if (call.FunctionName() == operators.Index || call.FunctionName() == operators.OptIndex) && len(call.Args()) == 2 {
		target := unwrapOptional(a.GetType(call.Args()[0].ID()))
		root, path, _, found := expressionPath(call.Args()[0])
		if found {
			segment := PathSegment{Dynamic: true}
			if index, ok := literalIndex(call.Args()[1]); ok {
				segment.Index = index
			}
			if target.Kind() == types.DynKind || target.Kind() == types.AnyKind || segment.Dynamic {
				c.addDynamic(id, root, append(path, segment), "index", a.GetType(id))
			}
		}
	}
}

func (c *DependencyContract) addOptSelectDependency(a *celast.AST, id int64, operand celast.Expr, field string) {
	root, path, dynamicBefore, found := expressionPath(operand)
	if !found {
		return
	}
	target := unwrapOptional(a.GetType(operand.ID()))
	if target.Kind() == types.StructKind && !dynamicBefore {
		c.Fields = append(c.Fields, FieldDependency{
			ExprID: id, Variable: root, Path: path, Field: field,
			Type: typeRef(unwrapOptional(a.GetType(id))),
		})
		return
	}
	if target.Kind() == types.StructKind || target.Kind() == types.DynKind || target.Kind() == types.AnyKind {
		c.addDynamic(id, root, append(path, PathSegment{Field: field}), "optional-select", a.GetType(id))
	}
}

func (c *DependencyContract) addDynamic(id int64, root string, path []PathSegment, operation string, t *types.Type) {
	if _, local := c.localVars[root]; local {
		return
	}
	c.Dynamic = append(c.Dynamic, DynamicAccessDependency{
		ExprID: id, Variable: root, Path: path, Operation: operation, Type: typeRef(t),
	})
}

func collectLocalVariables(a *celast.AST) map[string]struct{} {
	locals := map[string]struct{}{}
	celast.PostOrderVisit(a.Expr(), celast.NewExprVisitor(func(e celast.Expr) {
		if e.Kind() != celast.ComprehensionKind {
			return
		}
		comp := e.AsComprehension()
		locals[comp.IterVar()] = struct{}{}
		if comp.HasIterVar2() {
			locals[comp.IterVar2()] = struct{}{}
		}
		locals[comp.AccuVar()] = struct{}{}
	}))
	return locals
}

func expressionPath(e celast.Expr) (root string, path []PathSegment, dynamic bool, found bool) {
	switch e.Kind() {
	case celast.IdentKind:
		return normalizeSymbol(e.AsIdent()), nil, false, true
	case celast.SelectKind:
		sel := e.AsSelect()
		root, path, dynamic, found = expressionPath(sel.Operand())
		if !found {
			return "", nil, false, false
		}
		return root, append(path, PathSegment{Field: sel.FieldName()}), dynamic, true
	case celast.CallKind:
		call := e.AsCall()
		if (call.FunctionName() == operators.Index || call.FunctionName() == operators.OptIndex) &&
			((call.IsMemberFunction() && len(call.Args()) == 1) || (!call.IsMemberFunction() && len(call.Args()) == 2)) {
			target := call.Target()
			key := call.Args()[0]
			if !call.IsMemberFunction() {
				target = call.Args()[0]
				key = call.Args()[1]
			}
			root, path, _, found = expressionPath(target)
			if !found {
				return "", nil, false, false
			}
			segment := PathSegment{Dynamic: true}
			if index, ok := literalIndex(key); ok {
				segment.Index = index
				segment.Dynamic = false
			} else {
				dynamic = true
			}
			return root, append(path, segment), dynamic, true
		}
		if call.FunctionName() == operators.OptSelect && len(call.Args()) == 2 {
			if field, ok := literalString(call.Args()[1]); ok {
				root, path, dynamic, found = expressionPath(call.Args()[0])
				return root, append(path, PathSegment{Field: field}), dynamic, found
			}
		}
	}
	return "", nil, false, false
}

func literalString(e celast.Expr) (string, bool) {
	if e == nil || e.Kind() != celast.LiteralKind {
		return "", false
	}
	value, ok := e.AsLiteral().(types.String)
	return string(value), ok
}

func literalIndex(e celast.Expr) (string, bool) {
	if e == nil || e.Kind() != celast.LiteralKind {
		return "", false
	}
	switch value := e.AsLiteral().(type) {
	case types.String:
		return string(value), true
	case types.Int:
		return fmt.Sprintf("%d", int64(value)), true
	case types.Uint:
		return fmt.Sprintf("%d", uint64(value)), true
	case types.Bool:
		return fmt.Sprintf("%t", bool(value)), true
	default:
		return "", false
	}
}

func normalizeSymbol(name string) string { return strings.TrimPrefix(name, ".") }

func unwrapOptional(t *types.Type) *types.Type {
	if t != nil && t.Kind() == types.OpaqueKind && t.TypeName() == "optional_type" && len(t.Parameters()) == 1 {
		return t.Parameters()[0]
	}
	return t
}

func typeRef(t *types.Type) TypeRef {
	if t == nil {
		return TypeRef{}
	}
	ref := TypeRef{
		Kind:     uint(t.Kind()),
		Name:     t.TypeName(),
		Nullable: isNullableType(t),
	}
	if ref.Nullable {
		ref.Name = "wrapper(" + primitiveName(t.Kind()) + ")"
	}
	for _, parameter := range t.Parameters() {
		ref.Parameters = append(ref.Parameters, typeRef(parameter))
	}
	return ref
}

func isNullableType(t *types.Type) bool {
	switch t.Kind() {
	case types.BoolKind, types.BytesKind, types.DoubleKind, types.IntKind, types.StringKind, types.UintKind:
		return !t.IsExactType(types.NullType) && t.IsAssignableType(types.NullType)
	default:
		return false
	}
}

func primitiveName(kind types.Kind) string {
	switch kind {
	case types.BoolKind:
		return "bool"
	case types.BytesKind:
		return "bytes"
	case types.DoubleKind:
		return "double"
	case types.IntKind:
		return "int"
	case types.StringKind:
		return "string"
	case types.UintKind:
		return "uint"
	default:
		return ""
	}
}

func sameTypeRef(expected, actual TypeRef) bool {
	if expected.Kind != actual.Kind || expected.Name != actual.Name || expected.Nullable != actual.Nullable ||
		len(expected.Parameters) != len(actual.Parameters) {
		return false
	}
	for i := range expected.Parameters {
		if !sameTypeRef(expected.Parameters[i], actual.Parameters[i]) {
			return false
		}
	}
	return true
}

func formatTypeRef(ref TypeRef) string {
	name := ref.Name
	if name == "" {
		name = checker.FormatCELType(typeFromRefOrDyn(ref))
	}
	if len(ref.Parameters) == 0 {
		return name
	}
	parameters := make([]string, len(ref.Parameters))
	for i, parameter := range ref.Parameters {
		parameters[i] = formatTypeRef(parameter)
	}
	return fmt.Sprintf("%s(%s)", name, strings.Join(parameters, ", "))
}

func typeFromRefOrDyn(ref TypeRef) *types.Type {
	t, err := typeFromRef(ref)
	if err != nil {
		return types.DynType
	}
	return t
}

func typeFromRef(ref TypeRef) (*types.Type, error) {
	parameters := make([]*types.Type, len(ref.Parameters))
	for i, parameter := range ref.Parameters {
		var err error
		parameters[i], err = typeFromRef(parameter)
		if err != nil {
			return nil, err
		}
	}
	switch types.Kind(ref.Kind) {
	case types.ListKind:
		if len(parameters) != 1 {
			return nil, fmt.Errorf("invalid list type reference")
		}
		return types.NewListType(parameters[0]), nil
	case types.MapKind:
		if len(parameters) != 2 {
			return nil, fmt.Errorf("invalid map type reference")
		}
		return types.NewMapType(parameters[0], parameters[1]), nil
	case types.OpaqueKind:
		return types.NewOpaqueType(ref.Name, parameters...), nil
	case types.StructKind:
		return types.NewObjectType(ref.Name), nil
	case types.TypeKind:
		if len(parameters) == 1 {
			return types.NewTypeTypeWithParam(parameters[0]), nil
		}
		return types.TypeType, nil
	case types.TypeParamKind:
		return types.NewTypeParamType(ref.Name), nil
	default:
		t := primitiveType(types.Kind(ref.Kind))
		if t == nil {
			return nil, fmt.Errorf("unsupported type kind: %d", ref.Kind)
		}
		if ref.Nullable {
			return types.NewNullableType(t), nil
		}
		return t, nil
	}
}

func primitiveType(kind types.Kind) *types.Type {
	switch kind {
	case types.AnyKind:
		return types.AnyType
	case types.BoolKind:
		return types.BoolType
	case types.BytesKind:
		return types.BytesType
	case types.DoubleKind:
		return types.DoubleType
	case types.DurationKind:
		return types.DurationType
	case types.DynKind:
		return types.DynType
	case types.ErrorKind:
		return types.ErrorType
	case types.IntKind:
		return types.IntType
	case types.NullTypeKind:
		return types.NullType
	case types.StringKind:
		return types.StringType
	case types.TimestampKind:
		return types.TimestampType
	case types.UintKind:
		return types.UintType
	default:
		return nil
	}
}

func formatPathSegments(root string, path []PathSegment) string {
	parts := []string{root}
	for _, segment := range path {
		switch {
		case segment.Dynamic:
			parts = append(parts, "[]")
		case segment.Field != "":
			parts = append(parts, segment.Field)
		case segment.Index != "":
			parts = append(parts, "["+segment.Index+"]")
		}
	}
	return strings.Join(parts, ".")
}

func sortContract(contract *DependencyContract) {
	slices.SortFunc(contract.Variables, func(a, b VariableDependency) int {
		if a.ExprID != b.ExprID {
			return int(a.ExprID - b.ExprID)
		}
		return strings.Compare(a.Name, b.Name)
	})
	slices.SortFunc(contract.Fields, func(a, b FieldDependency) int {
		left := formatPathSegments(a.Variable, append(slices.Clone(a.Path), PathSegment{Field: a.Field})) + a.TypeName
		right := formatPathSegments(b.Variable, append(slices.Clone(b.Path), PathSegment{Field: b.Field})) + b.TypeName
		if a.ExprID != b.ExprID {
			return int(a.ExprID - b.ExprID)
		}
		return strings.Compare(left, right)
	})
	slices.SortFunc(contract.Functions, func(a, b FunctionDependency) int {
		if a.ExprID != b.ExprID {
			return int(a.ExprID - b.ExprID)
		}
		return strings.Compare(a.Name, b.Name)
	})
	for _, call := range contract.Functions {
		slices.SortFunc(call.Overloads, func(a, b OverloadContract) int { return strings.Compare(a.ID, b.ID) })
	}
	slices.SortFunc(contract.Dynamic, func(a, b DynamicAccessDependency) int {
		if a.ExprID != b.ExprID {
			return int(a.ExprID - b.ExprID)
		}
		return strings.Compare(formatPathSegments(a.Variable, a.Path), formatPathSegments(b.Variable, b.Path))
	})
}

type stringified interface{ signatureText() string }

func overloadMismatch(id int64, expected OverloadContract, actual *decls.OverloadDecl) DependencyMismatch {
	actualText := "<missing>"
	if actual != nil {
		args := make([]string, len(actual.ArgTypes()))
		for i, arg := range actual.ArgTypes() {
			args[i] = checker.FormatCELType(arg)
		}
		actualText = fmt.Sprintf("member=%t (%s) -> %s", actual.IsMemberFunction(),
			strings.Join(args, ", "), checker.FormatCELType(actual.ResultType()))
	}
	return DependencyMismatch{ExprID: id, Kind: "overload", Symbol: expected.ID, Expected: expected.signatureText(), Actual: actualText}
}

func makeMismatch(id int64, kind, symbol string, expected TypeRef, actual any) DependencyMismatch {
	actualText := "<missing>"
	switch value := actual.(type) {
	case nil:
	case *types.Type:
		if value != nil {
			actualText = checker.FormatCELType(value)
		}
	case stringified:
		actualText = value.signatureText()
	}
	return DependencyMismatch{ExprID: id, Kind: kind, Symbol: symbol, Expected: formatTypeRef(expected), Actual: actualText}
}

func missingOverloadMismatch(id int64, overload OverloadContract) DependencyMismatch {
	return DependencyMismatch{ExprID: id, Kind: "overload", Symbol: overload.ID, Expected: overload.signatureText(), Actual: "<missing>"}
}

func (o OverloadContract) signatureText() string {
	args := make([]string, len(o.ArgumentTypes))
	for i, arg := range o.ArgumentTypes {
		args[i] = formatTypeRef(arg)
	}
	return fmt.Sprintf("member=%t (%s) -> %s", o.Member, strings.Join(args, ", "), formatTypeRef(o.ResultType))
}

func overloadMatches(declared *decls.OverloadDecl, expected OverloadContract) bool {
	bindings := map[string]*types.Type{}
	if len(declared.ArgTypes()) != len(expected.ArgumentTypes) {
		return false
	}
	for i, declaredArg := range declared.ArgTypes() {
		if !inferTypeBinding(declaredArg, expected.ArgumentTypes[i], bindings) {
			return false
		}
	}
	return sameTypeRef(expected.ResultType, renderType(declared.ResultType(), bindings))
}

func inferTypeBinding(declaredType *types.Type, expected TypeRef, bindings map[string]*types.Type) bool {
	if declaredType.Kind() == types.TypeParamKind {
		if resolved, found := bindings[declaredType.TypeName()]; found {
			return sameTypeRef(typeRef(resolved), expected)
		}
		resolved, err := typeFromRef(expected)
		if err != nil {
			return false
		}
		bindings[declaredType.TypeName()] = resolved
		return true
	}
	if uint(declaredType.Kind()) != expected.Kind || declaredType.TypeName() != expected.Name ||
		isNullableType(declaredType) != expected.Nullable || len(declaredType.Parameters()) != len(expected.Parameters) {
		return false
	}
	for i, parameter := range declaredType.Parameters() {
		if !inferTypeBinding(parameter, expected.Parameters[i], bindings) {
			return false
		}
	}
	return true
}

func renderType(t *types.Type, bindings map[string]*types.Type) TypeRef {
	if t.Kind() == types.TypeParamKind {
		if resolved, found := bindings[t.TypeName()]; found {
			return typeRef(resolved)
		}
	}
	rendered := typeRef(t)
	rendered.Parameters = nil
	for _, parameter := range t.Parameters() {
		rendered.Parameters = append(rendered.Parameters, renderType(parameter, bindings))
	}
	return rendered
}
