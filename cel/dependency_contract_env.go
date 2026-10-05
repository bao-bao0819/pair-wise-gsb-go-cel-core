package cel

import (
	"cel.dev/cel-go/common/decls"
	"cel.dev/cel-go/common/types"
)

func (e *Env) resolveVariableType(name string) *types.Type {
	for _, variable := range e.Variables() {
		if variable.Name() == name {
			return variable.Type()
		}
	}
	if value, found := e.provider.FindIdent(name); found {
		if typeValue, ok := value.(*types.Type); ok {
			return typeValue
		}
		if typeValue, ok := value.Type().(*types.Type); ok {
			return typeValue
		}
	}
	if enumValue := e.provider.EnumValue(name); enumValue != nil && enumValue.Type() != types.ErrType {
		return types.IntType
	}
	return nil
}

func (e *Env) resolveFunction(name string) *decls.FunctionDecl {
	return e.functions[name]
}

func (e *Env) resolveFieldType(dependency FieldDependency) *types.Type {
	if dependency.TypeName != "" {
		return e.structFieldType(dependency.TypeName, dependency.Field)
	}
	current := e.resolveVariableType(dependency.Variable)
	if current == nil {
		return nil
	}
	for _, field := range dependency.Path {
		if field.Index != "" && !field.Dynamic {
			if current.Kind() != types.MapKind || len(current.Parameters()) != 2 {
				return nil
			}
			current = current.Parameters()[1]
			continue
		}
		if field.Dynamic {
			return nil
		}
		current = e.structFieldType(current.TypeName(), field.Field)
		if current == nil {
			return nil
		}
	}
	return e.structFieldType(current.TypeName(), dependency.Field)
}

func (e *Env) structFieldType(typeName, fieldName string) *types.Type {
	field, found := e.provider.FindStructFieldType(typeName, fieldName)
	if !found {
		return nil
	}
	return field.Type
}

func (e *Env) resolveDynamicType(dependency DynamicAccessDependency) *types.Type {
	current := e.resolveVariableType(dependency.Variable)
	if current == nil {
		return nil
	}
	for _, segment := range dependency.Path {
		current = unwrapOptional(current)
		switch {
		case segment.Dynamic:
			if (len(dependency.Path) == 1 && current.Kind() == types.DynKind) ||
				current.Kind() == types.AnyKind {
				return current
			}
			return nil
		case segment.Field != "":
			if current.Kind() != types.StructKind {
				return nil
			}
			current = e.structFieldType(current.TypeName(), segment.Field)
		case !segment.Dynamic && segment.Index != "":
			if current.Kind() != types.MapKind || len(current.Parameters()) != 2 {
				return nil
			}
			current = current.Parameters()[1]
		}
		if current == nil {
			return nil
		}
	}
	return current
}
