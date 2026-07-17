package plan

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

// maxAlignmentCells bounds the total quadratic work of sequence alignment. Plans
// above this boundary still receive an exact, redacted array-level change.
const maxAlignmentCells = 1 << 20

type Change struct {
	Path   string `json:"path"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

type differ struct {
	beforeCopies []Copy
	afterCopies  []Copy
	alignedCells int
}

func Diff(before, after Plan) ([]Change, error) {
	d := &differ{
		beforeCopies: append([]Copy(nil), before.Copies...),
		afterCopies:  append([]Copy(nil), after.Copies...),
	}

	// Plans are passed by value, but their slices still share backing arrays
	// with the caller. Redact every secret copy on private slices before any
	// value can reach a Change, including inserted and removed copies.
	before.Copies = append([]Copy(nil), before.Copies...)
	after.Copies = append([]Copy(nil), after.Copies...)
	for i := range before.Copies {
		if before.Copies[i].Secret {
			before.Copies[i].SourceDigest = "<redacted>"
		}
	}
	for i := range after.Copies {
		if after.Copies[i].Secret {
			after.Copies[i].SourceDigest = "<redacted>"
		}
	}

	left, err := decodeValue(before)
	if err != nil {
		return nil, err
	}
	right, err := decodeValue(after)
	if err != nil {
		return nil, err
	}
	return d.diffValue("", left, right), nil
}

func decodeValue(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func (d *differ) diffValue(path string, before, after any) []Change {
	// The root and copies array may be display-equal while carrying a changed
	// secret digest in the private typed plans retained by differ.
	equal := reflect.DeepEqual(before, after)
	if path != "" && path != "copies" && equal {
		return nil
	}
	leftObject, leftIsObject := before.(map[string]any)
	rightObject, rightIsObject := after.(map[string]any)
	if leftIsObject && rightIsObject {
		return d.diffObject(path, leftObject, rightObject)
	}
	leftSequence, leftIsSequence := before.([]any)
	rightSequence, rightIsSequence := after.([]any)
	if leftIsSequence && rightIsSequence {
		return d.diffSequence(path, leftSequence, rightSequence)
	}
	if equal {
		return nil
	}
	return []Change{{Path: path, Before: renderValue(before), After: renderValue(after)}}
}

func (d *differ) diffObject(path string, before, after map[string]any) []Change {
	keys := make([]string, 0, len(before)+len(after))
	seen := make(map[string]bool, len(before)+len(after))
	for key := range before {
		seen[key] = true
		keys = append(keys, key)
	}
	for key := range after {
		if !seen[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	var changes []Change
	for _, key := range keys {
		next := key
		if path != "" {
			next = path + "." + key
		}
		left, leftOK := before[key]
		right, rightOK := after[key]
		switch {
		case !leftOK:
			if sequence, ok := right.([]any); ok {
				changes = append(changes, d.diffSequence(next, nil, sequence)...)
			} else {
				changes = append(changes, Change{Path: next, After: renderValue(right)})
			}
		case !rightOK:
			if sequence, ok := left.([]any); ok {
				changes = append(changes, d.diffSequence(next, sequence, nil)...)
			} else {
				changes = append(changes, Change{Path: next, Before: renderValue(left)})
			}
		default:
			changes = append(changes, d.diffValue(next, left, right)...)
		}
	}
	return changes
}

type sequenceMatch struct {
	before int
	after  int
}

func (d *differ) diffSequence(path string, before, after []any) []Change {
	matches, bounded := d.exactSequenceMatches(before, after)
	if !bounded {
		if reflect.DeepEqual(before, after) {
			return d.largeSecretChanges(path)
		}
		return []Change{{Path: path, Before: renderValue(before), After: renderValue(after)}}
	}

	// Exact LCS matches are anchors. Items in each gap are paired in order as
	// substitutions; this is the part a pure LCS omits, and is what preserves a
	// local B -> B2 edit as a modification rather than remove-plus-insert.
	matches = append(matches, sequenceMatch{before: len(before), after: len(after)})
	oldStart, newStart := 0, 0
	var changes []Change
	for _, match := range matches {
		oldCount := match.before - oldStart
		newCount := match.after - newStart
		paired := oldCount
		if newCount < paired {
			paired = newCount
		}
		for offset := 0; offset < paired; offset++ {
			oldIndex := oldStart + offset
			newIndex := newStart + offset
			changes = append(changes, d.diffSequencePair(path, oldIndex, newIndex, before[oldIndex], after[newIndex])...)
		}
		for oldIndex := oldStart + paired; oldIndex < match.before; oldIndex++ {
			changes = append(changes, Change{
				Path:   sequencePath(path, oldIndex),
				Before: renderValue(before[oldIndex]),
			})
		}
		for newIndex := newStart + paired; newIndex < match.after; newIndex++ {
			changes = append(changes, Change{
				Path:  sequencePath(path, newIndex),
				After: renderValue(after[newIndex]),
			})
		}
		if match.before < len(before) {
			changes = append(changes, d.diffSequencePair(path, match.before, match.after, before[match.before], after[match.after])...)
		}
		oldStart, newStart = match.before+1, match.after+1
	}
	return changes
}

func (d *differ) diffSequencePair(path string, oldIndex, newIndex int, before, after any) []Change {
	changes := d.diffValue(sequencePath(path, newIndex), before, after)
	if path != "copies" || oldIndex >= len(d.beforeCopies) || newIndex >= len(d.afterCopies) {
		return changes
	}
	left, right := d.beforeCopies[oldIndex], d.afterCopies[newIndex]
	if left.Secret && right.Secret && left.SourceDigest != right.SourceDigest {
		changes = append(changes, Change{
			Path:   fmt.Sprintf("copies[%d].source_digest", newIndex),
			Before: "<secret>",
			After:  "<secret changed>",
		})
	}
	return changes
}

// exactSequenceMatches returns deterministic exact LCS anchors. Ties discard
// the earlier before-side occurrence first, which makes duplicate matching
// reproducible and leaves reorder evidence visible as removals and insertions.
// The shared cell budget bounds quadratic work across all nested plan arrays.
func (d *differ) exactSequenceMatches(before, after []any) ([]sequenceMatch, bool) {
	rows, columns := len(before)+1, len(after)+1
	remaining := maxAlignmentCells - d.alignedCells
	if rows > remaining/columns {
		return nil, false
	}
	d.alignedCells += rows * columns
	leftIDs, rightIDs := sequenceIDs(before, after)
	table := make([]int, rows*columns)
	at := func(i, j int) int { return i*columns + j }
	for i := len(before) - 1; i >= 0; i-- {
		for j := len(after) - 1; j >= 0; j-- {
			if leftIDs[i] == rightIDs[j] {
				table[at(i, j)] = table[at(i+1, j+1)] + 1
			} else if table[at(i+1, j)] >= table[at(i, j+1)] {
				table[at(i, j)] = table[at(i+1, j)]
			} else {
				table[at(i, j)] = table[at(i, j+1)]
			}
		}
	}

	matches := make([]sequenceMatch, 0, table[0])
	for i, j := 0, 0; i < len(before) && j < len(after); {
		switch {
		case leftIDs[i] == rightIDs[j]:
			matches = append(matches, sequenceMatch{before: i, after: j})
			i++
			j++
		case table[at(i+1, j)] >= table[at(i, j+1)]:
			i++
		default:
			j++
		}
	}
	return matches, true
}

// sequenceIDs interns canonical values once so the bounded dynamic program
// compares integers rather than repeatedly traversing large nested elements.
func sequenceIDs(before, after []any) ([]int, []int) {
	ids := make(map[string]int, len(before)+len(after))
	next := 1
	intern := func(sequence []any) []int {
		out := make([]int, len(sequence))
		for i, value := range sequence {
			key := renderValue(value)
			id, ok := ids[key]
			if !ok {
				id = next
				next++
				ids[key] = id
			}
			out[i] = id
		}
		return out
	}
	return intern(before), intern(after)
}

func (d *differ) largeSecretChanges(path string) []Change {
	if path != "copies" || len(d.beforeCopies) != len(d.afterCopies) {
		return nil
	}
	var changes []Change
	for i := range d.beforeCopies {
		left, right := d.beforeCopies[i], d.afterCopies[i]
		if left.Secret && right.Secret && left.SourceDigest != right.SourceDigest {
			changes = append(changes, Change{
				Path:   fmt.Sprintf("copies[%d].source_digest", i),
				Before: "<secret>",
				After:  "<secret changed>",
			})
		}
	}
	return changes
}

func sequencePath(path string, index int) string {
	return fmt.Sprintf("%s[%d]", path, index)
}

func renderValue(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
