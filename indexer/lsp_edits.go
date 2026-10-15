package indexer

import (
	"bytes"
	"fmt"

	"github.com/mertcikla/codeindex/graph"
	scip "github.com/scip-code/scip/bindings/go/scip"
)

type bodySpan struct{ start, end int }
type bodyEdit struct {
	before, after        *graph.Source
	oldBodies, newBodies []bodySpan
	changed              []bool
}

func newBodyEdit(before, after *graph.Source, old, current []bodySpan) (*bodyEdit, error) {
	if len(old) != len(current) {
		return nil, fmt.Errorf("function membership changed")
	}
	a, b := 0, 0
	changed := make([]bool, len(old))
	for i, span := range old {
		if !bytes.Equal(before.Text[a:span.start], after.Text[b:current[i].start]) {
			return nil, fmt.Errorf("text outside function bodies changed")
		}
		changed[i] = !bytes.Equal(before.Text[span.start:span.end], after.Text[current[i].start:current[i].end])
		a, b = span.end, current[i].end
	}
	if !bytes.Equal(before.Text[a:], after.Text[b:]) {
		return nil, fmt.Errorf("text outside function bodies changed")
	}
	return &bodyEdit{before: before, after: after, oldBodies: old, newBodies: current, changed: changed}, nil
}

func (e *bodyEdit) offset(old int) (int, bool) {
	delta := 0
	for i, b := range e.oldBodies {
		if old <= b.start {
			return old + delta, true
		}
		if old < b.end {
			if !e.changed[i] {
				return old + delta, true
			}
			return 0, false
		}
		delta = e.newBodies[i].end - b.end
	}
	return old + delta, true
}

func byteRange(src *graph.Source, start, end int) []int32 {
	sl, sc := src.Position(start)
	el, ec := src.Position(end)
	if sl == el {
		return []int32{int32(sl), int32(sc), int32(ec)}
	}
	return []int32{int32(sl), int32(sc), int32(el), int32(ec)}
}

func (e *bodyEdit) remap(rng []int32, encoding string) ([]int32, bool) {
	if len(rng) == 0 {
		return nil, true
	}
	a, err := scipAnchor(e.before, &scip.Occurrence{Range: rng}, encoding)
	if err != nil {
		return nil, false
	}
	start, ok := e.offset(int(a.StartByte))
	if !ok {
		return nil, false
	}
	end, ok := e.offset(int(a.EndByte))
	if !ok {
		return nil, false
	}
	return byteRange(e.after, start, end), true
}
