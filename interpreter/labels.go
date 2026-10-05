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

package interpreter

import (
	"fmt"
	"sort"
	"strings"

	"cel.dev/cel-go/common/types/ref"
)

// Label is an opaque security classification tag attached to a piece of data.
//
// Labels are identified by a root variable and an optional field path beneath it. Path
// segments are encoded as CEL qualifier values: a struct/map field is a string segment and a
// list index is an int64 segment. A label declared for path P taints the value at P and every
// value selected from P.
type Label struct {
	// Variable is the root input variable name.
	Variable string
	// Path holds the optional field or index qualifiers below the root variable.
	Path []any
}

// String renders the label as a CEL-like selector path.
func (l Label) String() string {
	var sb strings.Builder
	sb.WriteString(l.Variable)
	for _, seg := range l.Path {
		switch v := seg.(type) {
		case string:
			sb.WriteByte('.')
			sb.WriteString(v)
		case int64:
			fmt.Fprintf(&sb, "[%d]", v)
		case uint64:
			fmt.Fprintf(&sb, "[%d]", v)
		case bool:
			fmt.Fprintf(&sb, "[%t]", v)
		default:
			fmt.Fprintf(&sb, "[%v]", v)
		}
	}
	return sb.String()
}

// LabelKind classifies how a registered function overload interacts with labels.
type LabelKind int

const (
	// LabelPropagate is the default: the function result carries the union of its evaluated
	// argument labels.
	LabelPropagate LabelKind = iota
	// LabelSink marks the overload as a restricted sink whose arguments leave the trust
	// boundary. The host LabelSinkPolicy is consulted with the argument labels before the
	// function body runs; a rejection aborts evaluation and the body is never invoked.
	LabelSink
	// LabelSanitize marks the overload as an explicit de-classification function. Its result
	// carries no labels regardless of the labels on its arguments.
	LabelSanitize
)

// SinkCall describes a single invocation of a restricted sink function at the point where the
// host policy must make an admission decision.
type SinkCall struct {
	// Function is the function name as it appears in the expression.
	Function string
	// OverloadID identifies the matched overload.
	OverloadID string
	// ArgumentLabels holds the label set of each evaluated argument, in argument order. The
	// receiver (for member-style calls) is argument zero.
	ArgumentLabels [][]Label
}

// CombinedLabels returns the union of the argument label sets.
func (c SinkCall) CombinedLabels() []Label {
	seen := map[string]struct{}{}
	var out []Label
	for _, set := range c.ArgumentLabels {
		for _, l := range set {
			if _, ok := seen[l.String()]; ok {
				continue
			}
			seen[l.String()] = struct{}{}
			out = append(out, l)
		}
	}
	return out
}

// LabelSinkPolicy decides whether a call to a restricted sink is permitted.
//
// The policy runs before the function body is invoked. A non-nil error rejects the call:
// evaluation produces a CEL error wrapping the policy error and the sink body is never executed
// (its invocation count remains zero).
type LabelSinkPolicy func(call SinkCall) error

// LabelConfig configures explicit label tracking for a program.
//
// Label tracking is opt-in: a program without label configuration evaluates unchanged.
type LabelConfig struct {
	// InputLabels maps a root variable to the labels declared for its value and its fields.
	// Labels for ancestor paths automatically apply to descendant selections.
	InputLabels map[string][]Label

	// Strict rejects extension function overloads which have no declared propagation rule at
	// plan time. Built-in CEL operators and overloads always propagate labels and are never
	// rejected.
	Strict bool

	// SinkPolicy is consulted before any LabelSink overload is invoked.
	SinkPolicy LabelSinkPolicy

	// Overloads records the propagation rule for individual extension overload ids, keyed by
	// overload id or by function name as a function-wide default.
	Overloads map[string]LabelKind
}

// labelSet is a de-duplicated set of labels. The empty set is represented by nil so that
// propagation from unlabeled data allocates nothing.
type labelSet []Label

func unionLabelSets(sets ...labelSet) labelSet {
	seen := map[string]struct{}{}
	var out labelSet
	for _, set := range sets {
		for _, l := range set {
			key := l.String()
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, l)
		}
	}
	if len(out) <= 1 {
		return out
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// labelTracker carries the label state for a single evaluation.
//
// The tracker is created when evaluation begins and discarded when it finishes: it is never
// stored on the shared Program or on input objects. Comprehension bodies execute through the
// label-aware fold node, which manages per-iteration labels through pushed scopes.
type labelTracker struct {
	cfg *LabelConfig

	// nodeLabels records the result value and label set for each executed node. The value is
	// retained so control-flow nodes can determine which branch was selected without
	// re-evaluating the condition.
	nodeLabels map[int64]labeledNode

	// locals stores per-scope labels for comprehension and bind variables. Child scope
	// lookups fall through to parent scopes.
	localScopes []map[string]labelSet

	// condValues memoizes explicitly evaluated ternary conditions for the current evaluation
	// so conditionalAttribute.Resolve does not re-execute the condition.
	condValues map[int64]ref.Val
}

// labeledNode pairs an evaluated node's CEL value with its label set.
type labeledNode struct {
	val    ref.Val
	labels labelSet
}

func newLabelTracker(cfg *LabelConfig) *labelTracker {
	return &labelTracker{
		cfg:        cfg,
		nodeLabels: make(map[int64]labeledNode),
		condValues: make(map[int64]ref.Val),
	}
}

func (t *labelTracker) setCondValue(id int64, val ref.Val) {
	t.condValues[id] = val
}

func (t *labelTracker) condValue(id int64) (ref.Val, bool) {
	v, ok := t.condValues[id]
	return v, ok
}

func (t *labelTracker) setNode(id int64, labels labelSet) {
	t.nodeLabels[id] = labeledNode{labels: labels}
}

func (t *labelTracker) setNodeValue(id int64, val ref.Val, labels labelSet) {
	t.nodeLabels[id] = labeledNode{val: val, labels: labels}
}

func (t *labelTracker) node(id int64) labelSet {
	return t.nodeLabels[id].labels
}

func (t *labelTracker) rawValue(_ *ExecutionFrame, id int64) ref.Val {
	return t.nodeLabels[id].val
}

func (t *labelTracker) pushScope() {
	t.localScopes = append(t.localScopes, map[string]labelSet{})
}

func (t *labelTracker) popScope() {
	t.localScopes = t.localScopes[:len(t.localScopes)-1]
}

func (t *labelTracker) setLocal(name string, labels labelSet) {
	scope := t.localScopes[len(t.localScopes)-1]
	scope[name] = labels
}

func (t *labelTracker) local(name string) (labelSet, bool) {
	for i := len(t.localScopes) - 1; i >= 0; i-- {
		if labels, ok := t.localScopes[i][name]; ok {
			return labels, true
		}
	}
	return nil, false
}

// labelsForPath returns the labels declared for a root variable at the given qualifier path.
//
// A label at path P applies when the selected path equals P or extends P: selecting a field
// from a labeled object inherits the object's label. Labels declared for a sibling or for a
// more deeply nested field do not apply.
func (t *labelTracker) labelsForPath(variable string, path []any) labelSet {
	declared, ok := t.cfg.InputLabels[variable]
	if !ok {
		return nil
	}
	var out labelSet
	for _, l := range declared {
		if pathExtends(path, l.Path) {
			out = unionLabelSets(out, labelSet{l})
		}
	}
	return out
}

// pathExtends reports whether path equals or is a descendant of prefix.
func pathExtends(path, prefix []any) bool {
	if len(path) < len(prefix) {
		return false
	}
	for i, seg := range prefix {
		if path[i] != seg {
			return false
		}
	}
	return true
}

// attrResolution describes the root variable binding for an attribute.
//
// dynamic is true when the attribute has a dynamic qualifier (e.g. m[k]): the root is known
// but the exact field path is not, so labels fall back to the root variable's declared labels.
type attrResolution struct {
	root    string
	path    []any
	dynamic bool
}

func resolveAttribute(attr Attribute) attrResolution {
	named, ok := attr.(interface {
		CandidateVariableNames() []string
		Qualifiers() []Qualifier
	})
	if !ok {
		return attrResolution{}
	}
	quals := named.Qualifiers()
	path := make([]any, 0, len(quals))
	for _, q := range quals {
		cq, isConst := q.(ConstantQualifier)
		if !isConst {
			names := named.CandidateVariableNames()
			if len(names) == 0 {
				return attrResolution{}
			}
			return attrResolution{root: names[0], dynamic: true}
		}
		path = append(path, cq.Value().Value())
	}
	names := named.CandidateVariableNames()
	if len(names) == 0 {
		return attrResolution{path: path}
	}
	return attrResolution{root: names[0], path: path}
}

// overloadKind resolves the declared propagation rule for an overload.
func (cfg *LabelConfig) overloadKind(function, overload string) (LabelKind, bool) {
	if overload != "" {
		if kind, ok := cfg.Overloads[overload]; ok {
			return kind, true
		}
	}
	kind, ok := cfg.Overloads[function]
	return kind, ok
}
