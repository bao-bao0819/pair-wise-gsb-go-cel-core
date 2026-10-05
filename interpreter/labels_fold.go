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
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
)

// labeledFold instruments a comprehension. It reuses the pooled folder for value semantics
// (including mutable list/map accumulation and interrupt handling) and maintains a parallel
// per-evaluation label scope for the iteration and accumulator variables.
//
// The result carries:
//   - data labels of the iterated range and every accumulated value that survived,
//   - control labels of every evaluated loop condition which selected or rejected entries.
//
// Label scopes form a stack so that an iteration variable shadows an outer variable or an
// outer comprehension's variable of the same name.
type labeledFold struct {
	*evalFold
	prog *LabelProgram
}

func newLabeledFold(fold *evalFold, prog *LabelProgram) *labeledFold {
	return &labeledFold{evalFold: fold, prog: prog}
}

func (lf *labeledFold) Eval(ctx Activation) ref.Val {
	return EvalActivation(ctx, lf.Exec)
}

func (lf *labeledFold) Exec(frame *ExecutionFrame) ref.Val {
	st := frame.labelState()
	if st == nil {
		return lf.evalFold.Exec(frame)
	}

	st.pushLocalScope()
	rangeLabels := st.labelsOf(lf.iterRange.ID())
	// The accumulator starts from its (typically constant) initializer.
	st.setLocal(lf.accuVar, st.labelsOf(lf.accu.ID()))

	f := newFolder(lf.evalFold, frame)
	lfolder := &labeledFolder{folder: f, st: st, rangeLabels: rangeLabels}
	defer func() {
		// Result data labels plus all control conditions and range labels that influenced the
		// surviving result.
		final := st.nodeLabels[lf.result.ID()].add(lfolder.control).add(rangeLabels)
		st.setLabels(lf.id, final)
		releaseFolder(f)
		st.popLocalScope()
	}()

	foldRange := lf.iterRange.Exec(frame)
	if types.IsUnknownOrError(foldRange) {
		return foldRange
	}
	if sizer, ok := foldRange.(traits.Sizer); ok && sizer.Size() == types.IntZero {
		return f.evalResult()
	}

	var foldable traits.Foldable
	switch r := foldRange.(type) {
	case traits.Foldable:
		foldable = r
		if lf.iterVar2 == "" {
			_, f.listV1Fold = foldRange.(traits.Lister)
		}
	case traits.Mapper:
		foldable = types.ToFoldableMap(r)
	case traits.Lister:
		if lf.iterVar2 == "" {
			f.listV1Fold = true
		}
		foldable = types.ToFoldableList(r)
	default:
		if lf.iterVar2 != "" {
			return types.NewErrWithNodeID(lf.ID(), "unsupported comprehension range type: %T", foldRange)
		}
	}
	if foldable != nil {
		foldable.Fold(lfolder)
		return f.evalResult()
	}

	if !foldRange.Type().HasTrait(traits.IterableType) {
		return types.ValOrErr(foldRange, "got '%T', expected iterable type", foldRange)
	}
	return lfolder.foldIterable(foldRange.(traits.Iterable))
}

// labeledFolder wraps the value-carrying folder to maintain iteration labels. Values still
// resolve through the embedded folder; labels are maintained in the parallel label state.
type labeledFolder struct {
	folder      *folder
	st          *labelEvalState
	rangeLabels labelSet
	control     labelSet
}

// setIterLabels records the labels of the current iteration variables. The exact per-element
// source is not statically known for dynamic ranges, so the range's labels are used, which is a
// conservative approximation.
func (lf *labeledFolder) setIterLabels() {
	f := lf.folder
	// Bind the iteration variable explicitly even when the range is unlabeled: the variable
	// shadows any outer input of the same name regardless of whether it carries labels.
	iterLabels := lf.rangeLabels
	if iterLabels == nil {
		iterLabels = labelSet{}
	}
	lf.st.setLocal(f.iterVar, iterLabels)
	if f.iterVar2 != "" {
		lf.st.setLocal(f.iterVar2, iterLabels)
	}
}

// FoldEntry implements traits.Folder, mirroring folder.FoldEntry while tracking labels.
func (lf *labeledFolder) FoldEntry(key, val any) bool {
	f := lf.folder
	if f.listV1Fold {
		f.iterVar1Val = val
	} else {
		f.iterVar1Val = key
		f.iterVar2Val = val
	}
	lf.setIterLabels()

	cond := f.cond.Exec(f.frame)
	lf.control = lf.control.add(lf.st.labelsOf(f.cond.ID()))
	if f.interrupted {
		return false
	}
	if !f.exhaustive && cond != types.True {
		if isError(cond) {
			f.interrupted = true
		}
		return false
	}

	f.accuVal = f.step.Exec(f.frame)
	f.initialized = true
	lf.st.setLocal(f.accuVar, lf.st.labelsOf(f.step.ID()))
	if f.interruptable && f.frame.CheckInterrupt() {
		f.interrupted = true
		return false
	}
	return true
}

// foldIterable mirrors folder.foldIterable for iterable (non-foldable) ranges.
func (lf *labeledFolder) foldIterable(iterable traits.Iterable) ref.Val {
	f := lf.folder
	it := iterable.Iterator()
	for it.HasNext() == types.True {
		f.iterVar1Val = it.Next()
		lf.setIterLabels()

		cond := f.cond.Exec(f.frame)
		lf.control = lf.control.add(lf.st.labelsOf(f.cond.ID()))
		if f.interrupted {
			return f.evalResult()
		}
		if !f.exhaustive && cond != types.True {
			if isError(cond) {
				f.interrupted = true
			}
			return f.evalResult()
		}

		f.accuVal = f.step.Exec(f.frame)
		f.initialized = true
		lf.st.setLocal(f.accuVar, lf.st.labelsOf(f.step.ID()))
		if f.interruptable && f.frame.CheckInterrupt() {
			f.interrupted = true
			return f.evalResult()
		}
	}
	return f.evalResult()
}
