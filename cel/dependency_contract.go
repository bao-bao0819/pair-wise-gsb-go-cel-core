package cel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/decls"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/types"
)

const dependencyContractVersion = 1

// DependencyContract records only the environment information required by a checked AST.
type DependencyContract struct {
	Version            int                        `json:"version"`
	ExpressionID       int64                      `json:"expression_id"`
	Container          string                     `json:"container"`
	NamespaceUsed      bool                       `json:"namespace_used"`
	SignaturesComplete bool                       `json:"signatures_complete"`
	Variables          []DependencyVariable       `json:"variables"`
	Fields             []DependencyField          `json:"fields"`
	Overloads          []DependencyOverload       `json:"overloads"`
	DynamicTypes       []DependencyDynamicMessage `json:"dynamic_types"`
}

type DependencyVariable struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	ExprID  int64  `json:"expr_id"`
	Dynamic bool   `json:"dynamic,omitempty"`
}

type DependencyField struct {
	Message string `json:"message"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	ExprID  int64  `json:"expr_id"`
}

type DependencyOverload struct {
	Function      string   `json:"function"`
	ID            string   `json:"id"`
	Member        bool     `json:"member"`
	NonStrict     bool     `json:"non_strict"`
	OperandTrait  int      `json:"operand_trait"`
	ArgumentTypes []string `json:"argument_types"`
	ResultType    string   `json:"result_type"`
	ExprID        int64    `json:"expr_id"`
}

type DependencyDynamicMessage struct {
	Type   string                   `json:"type"`
	ExprID int64                    `json:"expr_id"`
	Fields []DependencyDynamicField `json:"fields"`
}

type DependencyDynamicField struct {
	Name string   `json:"name"`
	Type string   `json:"type"`
	Path []string `json:"path"`
}

// DependencyMismatch identifies one incompatible checked-AST dependency.
type DependencyMismatch struct {
	ExprID  int64  `json:"expr_id"`
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

// DependencyMismatchError is returned before planning when a checked AST depends on an incompatible Env.
type DependencyMismatchError struct {
	Mismatches []DependencyMismatch `json:"mismatches"`
}

func (e *DependencyMismatchError) Error() string {
	parts := make([]string, len(e.Mismatches))
	for i, mismatch := range e.Mismatches {
		parts[i] = fmt.Sprintf("expression %d: %s", mismatch.ExprID, mismatch.Message)
	}
	return "checked ast dependency contract is incompatible with the target env: " + strings.Join(parts, "; ")
}

// AsDependencyMismatches returns the detailed mismatches carried by a DependencyMismatchError.
func AsDependencyMismatches(err error) []DependencyMismatch {
	var mismatchErr *DependencyMismatchError
	if errors.As(err, &mismatchErr) {
		return mismatchErr.Mismatches
	}
	return nil
}

// ExportDependencyContract derives a conservative dependency contract from a checked Ast without
// access to its compiling Env. Variable, field, and observed call types are recorded, but overload
// declaration metadata must be exported through Env.ExportDependencyContract to prove signatures.
func ExportDependencyContract(a *Ast) (*DependencyContract, error) {
	if a == nil {
		return nil, fmt.Errorf("cannot export dependency contract from nil ast")
	}
	if !a.IsChecked() {
		return nil, fmt.Errorf("cannot export dependency contract from unchecked ast")
	}
	return exportDependencyContract(a.NativeRep(), nil)
}

// ExportDependencyContract derives a conservative dependency contract using the checked Env's
// declarations, namespace, and type provider.
func (e *Env) ExportDependencyContract(a *Ast) (*DependencyContract, error) {
	if a == nil {
		return nil, fmt.Errorf("cannot export dependency contract from nil ast")
	}
	if !a.IsChecked() {
		return nil, fmt.Errorf("cannot export dependency contract from unchecked ast")
	}
	return exportDependencyContract(a.NativeRep(), e)
}

// MarshalDependencyContract returns a versioned, deterministic JSON representation of a contract.
func MarshalDependencyContract(contract *DependencyContract) ([]byte, error) {
	if contract == nil {
		return nil, fmt.Errorf("cannot marshal nil dependency contract")
	}
	if contract.Version != dependencyContractVersion {
		return nil, fmt.Errorf("unsupported dependency contract version: %d", contract.Version)
	}
	normalized := *contract
	normalizeContract(&normalized)
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(normalized); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buffer.Bytes()), nil
}

// UnmarshalDependencyContract loads a versioned dependency contract from JSON.
func UnmarshalDependencyContract(data []byte) (*DependencyContract, error) {
	var envelope struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, err
	}
	if envelope.Version != dependencyContractVersion {
		return nil, fmt.Errorf("unsupported dependency contract version: %d", envelope.Version)
	}
	var contract DependencyContract
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&contract); err != nil {
		return nil, err
	}
	normalizeContract(&contract)
	return &contract, nil
}

// WithDependencyContract returns a copy of the checked Ast with the contract used by Env.Program.
func WithDependencyContract(a *Ast, contract *DependencyContract) (*Ast, error) {
	if a == nil {
		return nil, fmt.Errorf("cannot attach dependency contract to nil ast")
	}
	if !a.IsChecked() {
		return nil, fmt.Errorf("cannot attach dependency contract to unchecked ast")
	}
	if contract == nil {
		return nil, fmt.Errorf("cannot attach nil dependency contract")
	}
	if contract.ExpressionID != 0 && contract.ExpressionID != a.NativeRep().Expr().ID() {
		return nil, fmt.Errorf("dependency contract expression id %d does not match ast expression id %d", contract.ExpressionID, a.NativeRep().Expr().ID())
	}
	copied := *contract
	normalizeContract(&copied)
	return &Ast{source: a.source, impl: a.impl, loadErr: a.loadErr, contract: &copied}, nil
}

// CheckCompatibility compares a checked AST's actual dependencies with declarations in the target Env.
//
// Matching signatures do not prove that function implementations behave equivalently. The contract
// intentionally excludes function pointers and runtime addresses.
func (contract *DependencyContract) CheckCompatibility(env *Env) error {
	if contract == nil {
		return nil
	}
	if env == nil {
		return fmt.Errorf("cannot check dependency contract against nil env")
	}
	if contract.NamespaceUsed && contract.Container != env.Container.Name() {
		return &DependencyMismatchError{Mismatches: []DependencyMismatch{{
			ExprID:  0,
			Kind:    "namespace",
			Path:    "",
			Message: fmt.Sprintf("container changed from %q to %q", contract.Container, env.Container.Name()),
		}}}
	}
	vars := make(map[string]*decls.VariableDecl, len(env.Variables()))
	for _, variable := range env.Variables() {
		vars[variable.Name()] = variable
	}
	functions := env.Functions()
	mismatches := []DependencyMismatch{}
	for _, dependency := range contract.Variables {
		variable, found := vars[dependency.Name]
		if !found {
			mismatches = append(mismatches, mismatch(dependency.ExprID, "variable", dependency.Name, "variable is missing from target env"))
			continue
		}
		if typeString(variable.Type()) != dependency.Type {
			mismatches = append(mismatches, mismatch(dependency.ExprID, "variable", dependency.Name, formatTypeDiff(dependency.Type, typeString(variable.Type()))))
		}
	}
	for _, field := range contract.Fields {
		fieldType, found := env.provider.FindStructFieldType(field.Message, field.Name)
		if !found || fieldType == nil {
			mismatches = append(mismatches, mismatch(field.ExprID, "field", field.Message+"."+field.Name, "message field is missing from target env"))
			continue
		}
		if typeString(fieldType.Type) != field.Type {
			mismatches = append(mismatches, mismatch(field.ExprID, "field", field.Message+"."+field.Name, formatTypeDiff(field.Type, typeString(fieldType.Type))))
		}
	}
	for _, overload := range contract.Overloads {
		if !contract.SignaturesComplete {
			mismatches = append(mismatches, mismatch(overload.ExprID, "overload", overload.Function+"#"+overload.ID, "contract was exported without declaration metadata, so selected overload signature cannot be proven compatible"))
			continue
		}
		function, found := functions[overload.Function]
		if !found {
			mismatches = append(mismatches, mismatch(overload.ExprID, "overload", overload.Function+"#"+overload.ID, "function is missing from target env"))
			continue
		}
		selected := findOverload(function, overload.ID)
		if selected == nil {
			mismatches = append(mismatches, mismatch(overload.ExprID, "overload", overload.Function+"#"+overload.ID, "selected overload is missing from target env"))
			continue
		}
		mismatches = append(mismatches, compareOverload(overload, selected)...)
	}
	for _, dynamicType := range contract.DynamicTypes {
		mismatches = append(mismatches, compareDynamicType(env, dynamicType)...)
	}
	if len(mismatches) > 0 {
		sortMismatches(mismatches)
		return &DependencyMismatchError{Mismatches: mismatches}
	}
	return nil
}

type dependencyContractBuilder struct {
	provider           types.Provider
	impl               *celast.AST
	locals             map[string]int64
	vars               map[string]DependencyVariable
	fields             map[string]DependencyField
	overloads          map[string]DependencyOverload
	dynamic            map[string]*DependencyDynamicMessage
	container          string
	functions          map[string]*decls.FunctionDecl
	errs               []error
	namespaceUsed      bool
	signaturesComplete bool
}

func exportDependencyContract(a *celast.AST, env *Env) (*DependencyContract, error) {
	builder := &dependencyContractBuilder{
		container: "",
		impl:      a,
		locals:    map[string]int64{},
		vars:      map[string]DependencyVariable{},
		fields:    map[string]DependencyField{},
		overloads: map[string]DependencyOverload{},
		dynamic:   map[string]*DependencyDynamicMessage{},
	}
	if env != nil {
		builder.container = env.Container.Name()
		builder.provider = env.provider
		builder.functions = env.Functions()
		builder.signaturesComplete = true
	}
	root := a.Expr()
	collectLocals(root, builder.locals)
	celast.PostOrderVisit(root, celast.NewExprVisitor(func(e celast.Expr) {
		if e.Kind() == celast.IdentKind {
			builder.addIdent(e)
		}
		if e.Kind() == celast.SelectKind {
			builder.addSelect(e)
		}
		if e.Kind() == celast.StructKind {
			builder.addStruct(e)
		}
		if e.Kind() == celast.CallKind {
			builder.addCall(e)
		}
	}))
	if len(builder.errs) > 0 {
		return nil, errors.Join(builder.errs...)
	}
	contract := &DependencyContract{
		Version:            dependencyContractVersion,
		ExpressionID:       root.ID(),
		Container:          builder.container,
		NamespaceUsed:      builder.namespaceUsed,
		SignaturesComplete: env != nil,
	}
	for _, variable := range builder.vars {
		contract.Variables = append(contract.Variables, variable)
	}
	for _, field := range builder.fields {
		contract.Fields = append(contract.Fields, field)
	}
	for _, overload := range builder.overloads {
		contract.Overloads = append(contract.Overloads, overload)
	}
	for _, dynamicType := range builder.dynamic {
		contract.DynamicTypes = append(contract.DynamicTypes, *dynamicType)
	}
	normalizeContract(contract)
	return contract, nil
}

func (b *dependencyContractBuilder) addIdent(e celast.Expr) {
	name := strings.TrimPrefix(e.AsIdent(), ".")
	if _, local := b.locals[name]; local {
		return
	}
	refInfo, found := b.impl.ReferenceMap()[e.ID()]
	if found && len(refInfo.OverloadIDs) > 0 {
		return
	}
	if !strings.HasPrefix(e.AsIdent(), ".") {
		b.namespaceUsed = true
	}
	b.addVariable(name, b.impl.GetType(e.ID()), e.ID(), false)
}

func (b *dependencyContractBuilder) addVariable(name string, variableType *types.Type, exprID int64, dynamic bool) {
	if existing, exists := b.vars[name]; exists {
		if dynamic {
			existing.Dynamic = true
			b.vars[name] = existing
		}
		return
	}
	b.vars[name] = DependencyVariable{Name: name, Type: typeString(variableType), ExprID: exprID, Dynamic: dynamic}
}

func (b *dependencyContractBuilder) addSelect(e celast.Expr) {
	selectExpr := e.AsSelect()
	operand := selectExpr.Operand()
	_, found := rootIdentifier(operand, b.locals)
	if selectExpr.IsTestOnly() {
		found = true
	}
	if found {
		operandType := maybeUnwrapOptional(b.impl.GetType(operand.ID()))
		if operandType.Kind() == types.StructKind {
			b.addField(operandType.TypeName(), selectExpr.FieldName(), maybeUnwrapOptional(b.impl.GetType(e.ID())), e.ID())
		} else if isDynamicType(operandType) {
			b.markDynamicAccess(operand, e.ID())
		}
	}
}

func (b *dependencyContractBuilder) addStruct(e celast.Expr) {
	structExpr := e.AsStruct()
	messageType := maybeUnwrapOptional(b.impl.GetType(e.ID()))
	if messageType.Kind() != types.StructKind {
		return
	}
	for _, fieldExpr := range structExpr.Fields() {
		field := fieldExpr.AsStructField()
		fieldType := b.impl.GetType(field.Value().ID())
		b.addField(messageType.TypeName(), field.Name(), fieldType, fieldExpr.ID())
	}
}

func (b *dependencyContractBuilder) addCall(e celast.Expr) {
	call := e.AsCall()
	if call.FunctionName() == operators.OptSelect && len(call.Args()) == 2 {
		operand := call.Args()[0]
		fieldName, isString := maybeUnwrapString(call.Args()[1])
		if isString {
			_, found := rootIdentifier(operand, b.locals)
			if selectType := maybeUnwrapOptional(b.impl.GetType(operand.ID())); selectType.Kind() == types.StructKind {
				found = true
			}
			if found {
				operandType := maybeUnwrapOptional(b.impl.GetType(operand.ID()))
				if operandType.Kind() == types.StructKind {
					b.addField(operandType.TypeName(), fieldName, maybeUnwrapOptional(b.impl.GetType(e.ID())), e.ID())
				} else if isDynamicType(operandType) {
					b.markDynamicAccess(operand, e.ID())
				}
			}
		}
	}
	if call.FunctionName() == operators.Index || call.FunctionName() == operators.OptIndex {
		operand := call.Target()
		keyIndex := 0
		if (operand == nil || operand.Kind() == celast.UnspecifiedExprKind) && len(call.Args()) == 2 {
			operand = call.Args()[0]
			keyIndex = 1
		}
		if operand != nil && len(call.Args()) > keyIndex && !isConstantKey(call.Args()[keyIndex]) {
			rootName, found := rootIdentifier(operand, b.locals)
			if found {
				b.addDynamicAccess(rootName, maybeUnwrapOptional(b.impl.GetType(operand.ID())), e.ID())
			}
		}
		if operand != nil && len(call.Args()) > keyIndex && call.Args()[keyIndex].Kind() == celast.LiteralKind {
			if _, found := rootIdentifier(operand, b.locals); found {
				operandType := maybeUnwrapOptional(b.impl.GetType(operand.ID()))
				if operandType.Kind() == types.StructKind {
					if fieldName, isString := maybeUnwrapString(call.Args()[keyIndex]); isString {
						b.addField(operandType.TypeName(), fieldName, maybeUnwrapOptional(b.impl.GetType(e.ID())), e.ID())
					}
				}
			}
		}
	}
	refInfo, found := b.impl.ReferenceMap()[e.ID()]
	if !found || len(refInfo.OverloadIDs) == 0 {
		return
	}
	function := b.functions[strings.TrimPrefix(call.FunctionName(), ".")]
	if function != nil && !strings.HasPrefix(call.FunctionName(), ".") && !strings.HasPrefix(call.FunctionName(), "_") {
		b.namespaceUsed = true
	}
	observedArgs := []*types.Type{}
	if call.Target() != nil && call.Target().Kind() != celast.UnspecifiedExprKind {
		observedArgs = append(observedArgs, b.impl.GetType(call.Target().ID()))
	}
	for _, arg := range call.Args() {
		observedArgs = append(observedArgs, b.impl.GetType(arg.ID()))
	}
	observedResult := b.impl.GetType(e.ID())
	for _, overloadID := range refInfo.OverloadIDs {
		if function == nil {
			b.signaturesComplete = false
			b.addOverload(call.FunctionName(), overloadID, call.IsMemberFunction(), observedArgs, observedResult, false, 0, e.ID())
			continue
		}
		selected := findOverload(function, overloadID)
		if selected == nil {
			b.errs = append(b.errs, fmt.Errorf("expression %d: selected overload %q is missing from the env", e.ID(), overloadID))
			continue
		}
		b.addOverload(function.Name(), overloadID, selected.IsMemberFunction(), observedArgs, observedResult, selected.IsNonStrict(), selected.OperandTrait(), e.ID())
	}
}

func (b *dependencyContractBuilder) addField(message, name string, fieldType *types.Type, exprID int64) {
	key := message + "\x00" + name
	if _, exists := b.fields[key]; exists {
		return
	}
	b.fields[key] = DependencyField{Message: message, Name: name, Type: typeString(fieldType), ExprID: exprID}
}

func (b *dependencyContractBuilder) addOverload(function, overloadID string, member bool, args []*types.Type, resultType *types.Type, nonStrict bool, operandTrait int, exprID int64) {
	key := function + "\x00" + overloadID
	if _, exists := b.overloads[key]; exists {
		return
	}
	argTypes := make([]string, len(args))
	for i, argType := range args {
		argTypes[i] = typeString(argType)
	}
	b.overloads[key] = DependencyOverload{
		Function:      strings.TrimPrefix(function, "."),
		ID:            overloadID,
		Member:        member,
		NonStrict:     nonStrict,
		OperandTrait:  operandTrait,
		ArgumentTypes: argTypes,
		ResultType:    typeString(resultType),
		ExprID:        exprID,
	}
}

func (b *dependencyContractBuilder) addDynamicAccess(rootName string, rootType *types.Type, exprID int64) {
	if isDynamicType(rootType) {
		b.addVariable(rootName, types.DynType, exprID, true)
		return
	}
	if rootType.Kind() == types.MapKind {
		b.addVariable(rootName, rootType, exprID, true)
		return
	}
	if rootType.Kind() != types.StructKind {
		return
	}
	if b.provider == nil {
		b.errs = append(b.errs, fmt.Errorf("expression %d: cannot export conservative dependency for dynamic access to %q without a type provider", exprID, rootType.TypeName()))
		return
	}
	if err := b.snapshotMessage(rootType.TypeName(), exprID, nil, map[string]bool{}); err != nil {
		b.errs = append(b.errs, err)
	}
}

func (b *dependencyContractBuilder) markDynamicAccess(operand celast.Expr, exprID int64) {
	rootName, found := rootIdentifier(operand, b.locals)
	if !found {
		return
	}
	b.addVariable(rootName, types.DynType, exprID, true)
}

func isDynamicType(t *types.Type) bool {
	return t != nil && (t.Kind() == types.DynKind || t.Kind() == types.AnyKind || t.Kind() == types.TypeParamKind)
}

func isConstantKey(e celast.Expr) bool {
	if e.Kind() != celast.LiteralKind {
		return false
	}
	switch e.AsLiteral().Type() {
	case types.StringType, types.IntType, types.UintType, types.BoolType:
		return true
	default:
		return false
	}
}

func (b *dependencyContractBuilder) snapshotMessage(messageName string, exprID int64, path []string, seen map[string]bool) error {
	if seen[messageName] {
		return nil
	}
	seen[messageName] = true
	names, found := b.provider.FindStructFieldNames(messageName)
	if !found {
		return fmt.Errorf("message type %q is missing from the type provider", messageName)
	}
	dynamic := b.dynamic[messageName]
	if dynamic == nil {
		dynamic = &DependencyDynamicMessage{Type: messageName, ExprID: exprID}
		b.dynamic[messageName] = dynamic
	}
	for _, fieldName := range names {
		fieldType, found := b.provider.FindStructFieldType(messageName, fieldName)
		if !found || fieldType == nil {
			return fmt.Errorf("message field %q.%q is missing from the type provider", messageName, fieldName)
		}
		fieldPath := append(append([]string{}, path...), fieldName)
		dynamic.Fields = append(dynamic.Fields, DependencyDynamicField{Name: fieldName, Type: typeString(fieldType.Type), Path: fieldPath})
		unwrapped := maybeUnwrapOptional(fieldType.Type)
		if unwrapped.Kind() == types.StructKind {
			if err := b.snapshotMessage(unwrapped.TypeName(), exprID, fieldPath, seen); err != nil {
				return err
			}
		}
	}
	return nil
}

func collectLocals(e celast.Expr, locals map[string]int64) {
	if e == nil {
		return
	}
	switch e.Kind() {
	case celast.ComprehensionKind:
		comprehension := e.AsComprehension()
		locals[comprehension.AccuVar()] = e.ID()
		locals[comprehension.IterVar()] = e.ID()
		if comprehension.HasIterVar2() {
			locals[comprehension.IterVar2()] = e.ID()
		}
		collectLocals(comprehension.IterRange(), locals)
		collectLocals(comprehension.AccuInit(), locals)
		collectLocals(comprehension.LoopCondition(), locals)
		collectLocals(comprehension.LoopStep(), locals)
		collectLocals(comprehension.Result(), locals)
	case celast.CallKind:
		call := e.AsCall()
		if call.IsMemberFunction() {
			collectLocals(call.Target(), locals)
		}
		for _, arg := range call.Args() {
			collectLocals(arg, locals)
		}
	case celast.SelectKind:
		collectLocals(e.AsSelect().Operand(), locals)
	case celast.ListKind:
		for _, element := range e.AsList().Elements() {
			collectLocals(element, locals)
		}
	case celast.MapKind:
		for _, entry := range e.AsMap().Entries() {
			mapEntry := entry.AsMapEntry()
			collectLocals(mapEntry.Key(), locals)
			collectLocals(mapEntry.Value(), locals)
		}
	case celast.StructKind:
		for _, field := range e.AsStruct().Fields() {
			collectLocals(field.AsStructField().Value(), locals)
		}
	}
}

func rootIdentifier(e celast.Expr, locals map[string]int64) (string, bool) {
	for e != nil {
		switch e.Kind() {
		case celast.IdentKind:
			name := strings.TrimPrefix(e.AsIdent(), ".")
			_, local := locals[name]
			return name, !local
		case celast.SelectKind:
			e = e.AsSelect().Operand()
		default:
			return "", false
		}
	}
	return "", false
}

func maybeUnwrapString(e celast.Expr) (string, bool) {
	if e.Kind() != celast.LiteralKind {
		return "", false
	}
	value, ok := e.AsLiteral().Value().(string)
	return value, ok
}

func maybeUnwrapOptional(t *types.Type) *types.Type {
	for t != nil && t.Kind() == types.OpaqueKind && t.TypeName() == "optional_type" && len(t.Parameters()) == 1 {
		t = t.Parameters()[0]
	}
	return t
}

func typeString(t *types.Type) string {
	if t == nil {
		return ""
	}
	return t.String()
}

func normalizeContract(contract *DependencyContract) {
	if contract.Version == 0 {
		contract.Version = dependencyContractVersion
	}
	sort.Slice(contract.Variables, func(i, j int) bool { return contract.Variables[i].Name < contract.Variables[j].Name })
	sort.Slice(contract.Fields, func(i, j int) bool {
		left := contract.Fields[i].Message + "." + contract.Fields[i].Name
		right := contract.Fields[j].Message + "." + contract.Fields[j].Name
		return left < right
	})
	sort.Slice(contract.Overloads, func(i, j int) bool {
		left := contract.Overloads[i].Function + "#" + contract.Overloads[i].ID
		right := contract.Overloads[j].Function + "#" + contract.Overloads[j].ID
		return left < right
	})
	sort.Slice(contract.DynamicTypes, func(i, j int) bool { return contract.DynamicTypes[i].Type < contract.DynamicTypes[j].Type })
	for _, dynamicType := range contract.DynamicTypes {
		sort.Slice(dynamicType.Fields, func(i, j int) bool {
			return strings.Join(dynamicType.Fields[i].Path, ".") < strings.Join(dynamicType.Fields[j].Path, ".")
		})
		for i := range dynamicType.Fields {
			dynamicType.Fields[i].Path = slices.Clone(dynamicType.Fields[i].Path)
		}
	}
	for i := range contract.Overloads {
		contract.Overloads[i].ArgumentTypes = slices.Clone(contract.Overloads[i].ArgumentTypes)
	}
}

func findOverload(function *decls.FunctionDecl, overloadID string) *decls.OverloadDecl {
	for _, overload := range function.OverloadDecls() {
		if overload.ID() == overloadID {
			return overload
		}
	}
	return nil
}

func compareOverload(expected DependencyOverload, actual *decls.OverloadDecl) []DependencyMismatch {
	var mismatches []DependencyMismatch
	path := expected.Function + "#" + expected.ID
	if actual.IsMemberFunction() != expected.Member {
		mismatches = append(mismatches, mismatch(expected.ExprID, "overload", path+".member", fmt.Sprintf("member flag changed from %t to %t", expected.Member, actual.IsMemberFunction())))
	}
	if actual.IsNonStrict() != expected.NonStrict {
		mismatches = append(mismatches, mismatch(expected.ExprID, "overload", path+".non_strict", fmt.Sprintf("non-strict flag changed from %t to %t", expected.NonStrict, actual.IsNonStrict())))
	}
	if actual.OperandTrait() != expected.OperandTrait {
		mismatches = append(mismatches, mismatch(expected.ExprID, "overload", path+".operand_trait", fmt.Sprintf("operand trait changed from %d to %d", expected.OperandTrait, actual.OperandTrait())))
	}
	actualArgs := actual.ArgTypes()
	if len(actualArgs) != len(expected.ArgumentTypes) {
		mismatches = append(mismatches, mismatch(expected.ExprID, "overload", path+".arguments", fmt.Sprintf("argument count changed from %d to %d", len(expected.ArgumentTypes), len(actualArgs))))
	} else {
		instantiation := map[string]string{}
		argsCompatible := true
		for i, declaredArg := range actualArgs {
			var compatible bool
			compatible, instantiation = typeParameterInstantiation(typeString(declaredArg), expected.ArgumentTypes[i], instantiation)
			if !compatible {
				argsCompatible = false
				mismatches = append(mismatches, mismatch(expected.ExprID, "overload", fmt.Sprintf("%s.arguments[%d]", path, i), formatTypeDiff(expected.ArgumentTypes[i], typeString(declaredArg))))
			}
		}
		if argsCompatible && !instantiatedTypeEquals(typeString(actual.ResultType()), expected.ResultType, instantiation) {
			mismatches = append(mismatches, mismatch(expected.ExprID, "overload", path+".result", formatTypeDiff(expected.ResultType, typeString(actual.ResultType()))))
		}
	}
	return mismatches
}

func typeParameterInstantiation(declared, observed string, instantiation map[string]string) (bool, map[string]string) {
	if declared == "" || observed == "" {
		return false, instantiation
	}
	if name, isParam := typeParameterName(declared); isParam {
		if existing, found := instantiation[name]; found && existing != observed {
			return false, instantiation
		}
		instantiation[name] = observed
		return true, instantiation
	}
	if declared == observed {
		return true, instantiation
	}
	_, declaredName, declaredParams, declaredOK := splitTypeString(declared)
	_, observedName, observedParams, observedOK := splitTypeString(observed)
	if !declaredOK || !observedOK || declaredName != observedName || len(declaredParams) != len(observedParams) {
		return false, instantiation
	}
	for i := range declaredParams {
		var compatible bool
		compatible, instantiation = typeParameterInstantiation(declaredParams[i], observedParams[i], instantiation)
		if !compatible {
			return false, instantiation
		}
	}
	return true, instantiation
}

func instantiatedTypeEquals(declared, observed string, instantiation map[string]string) bool {
	if declared == "" || observed == "" {
		return false
	}
	if name, isParam := typeParameterName(declared); isParam {
		resolved, found := instantiation[name]
		return found && resolved == observed
	}
	if declared == observed {
		return true
	}
	_, declaredName, declaredParams, declaredOK := splitTypeString(declared)
	_, observedName, observedParams, observedOK := splitTypeString(observed)
	if !declaredOK || !observedOK || declaredName != observedName || len(declaredParams) != len(observedParams) {
		return false
	}
	for i := range declaredParams {
		if !instantiatedTypeEquals(declaredParams[i], observedParams[i], instantiation) {
			return false
		}
	}
	return true
}

func typeParameterName(typeName string) (string, bool) {
	return strings.TrimSuffix(strings.TrimPrefix(typeName, "<"), ">"), strings.HasPrefix(typeName, "<") && strings.HasSuffix(typeName, ">")
}

func splitTypeString(typeName string) (string, string, []string, bool) {
	open := strings.Index(typeName, "(")
	if open < 0 || !strings.HasSuffix(typeName, ")") {
		return "", typeName, nil, true
	}
	name := typeName[:open]
	parameters := splitTopLevelParameters(typeName[open+1 : len(typeName)-1])
	if len(parameters) == 0 {
		return "", "", nil, false
	}
	return name, name, parameters, true
}

func splitTopLevelParameters(value string) []string {
	var parameters []string
	depth := 0
	start := 0
	for i, r := range value {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parameters = append(parameters, strings.TrimSpace(value[start:i]))
				start = i + 1
			}
		}
	}
	return append(parameters, strings.TrimSpace(value[start:]))
}

func compareDynamicType(env *Env, expected DependencyDynamicMessage) []DependencyMismatch {
	if _, found := env.provider.FindStructType(expected.Type); !found {
		return []DependencyMismatch{mismatch(expected.ExprID, "dynamic", expected.Type, "dynamic message type is missing from target env")}
	}
	mismatches := []DependencyMismatch{}
	for _, field := range expected.Fields {
		fieldType, found := env.provider.FindStructFieldType(expected.Type, field.Name)
		if !found || fieldType == nil {
			mismatches = append(mismatches, mismatch(expected.ExprID, "dynamic", strings.Join(append([]string{expected.Type}, field.Path...), "."), "dynamic message field is missing from target env"))
			continue
		}
		actualType := typeString(fieldType.Type)
		if actualType != field.Type {
			mismatches = append(mismatches, mismatch(expected.ExprID, "dynamic", strings.Join(append([]string{expected.Type}, field.Path...), "."), formatTypeDiff(field.Type, actualType)))
		}
	}
	return mismatches
}

func mismatch(exprID int64, kind, path, message string) DependencyMismatch {
	return DependencyMismatch{ExprID: exprID, Kind: kind, Path: path, Message: message}
}

func formatTypeDiff(expected, actual string) string {
	return fmt.Sprintf("type changed from %s to %s", expected, actual)
}

func sortMismatches(mismatches []DependencyMismatch) {
	sort.Slice(mismatches, func(i, j int) bool {
		if mismatches[i].ExprID != mismatches[j].ExprID {
			return mismatches[i].ExprID < mismatches[j].ExprID
		}
		if mismatches[i].Kind != mismatches[j].Kind {
			return mismatches[i].Kind < mismatches[j].Kind
		}
		return mismatches[i].Path < mismatches[j].Path
	})
}
