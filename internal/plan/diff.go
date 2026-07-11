package plan

import (
	"encoding/json"
	"fmt"
	"sort"
)

type Change struct {
	Path   string `json:"path"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

func Diff(before, after Plan) ([]Change, error) {
	secretChanges := []Change{}
	for i := 0; i < len(before.Copies) && i < len(after.Copies); i++ {
		if before.Copies[i].Secret && after.Copies[i].Secret && before.Copies[i].SourceDigest != after.Copies[i].SourceDigest {
			secretChanges = append(secretChanges, Change{fmt.Sprintf("copies[%d].source_digest", i), "<secret>", "<secret changed>"})
		}
		if before.Copies[i].Secret {
			before.Copies[i].SourceDigest = "<redacted>"
		}
		if after.Copies[i].Secret {
			after.Copies[i].SourceDigest = "<redacted>"
		}
	}
	left, err := flatten(before)
	if err != nil {
		return nil, err
	}
	right, err := flatten(after)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for key := range left {
		keys[key] = true
	}
	for key := range right {
		keys[key] = true
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	changes := []Change{}
	for _, key := range ordered {
		if left[key] != right[key] {
			changes = append(changes, Change{key, left[key], right[key]})
		}
	}
	return append(changes, secretChanges...), nil
}
func flatten(value any) (map[string]string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	out := map[string]string{}
	flattenValue(out, "", decoded)
	return out, nil
}
func flattenValue(out map[string]string, path string, value any) {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			next := key
			if path != "" {
				next = path + "." + key
			}
			flattenValue(out, next, typed[key])
		}
	case []any:
		if len(typed) == 0 {
			out[path] = "[]"
		}
		for i, item := range typed {
			flattenValue(out, fmt.Sprintf("%s[%d]", path, i), item)
		}
	default:
		raw, _ := json.Marshal(value)
		out[path] = string(raw)
	}
}
