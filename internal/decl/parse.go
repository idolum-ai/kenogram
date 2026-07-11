// Package decl parses and validates Kenogram declarations.
package decl

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

type table map[string]any

// Parse parses the deliberately constrained TOML subset and decodes schema v1.
func Parse(data []byte) (Declaration, error) {
	if !utf8.Valid(data) {
		return Declaration{}, fmt.Errorf("declaration is not valid UTF-8")
	}
	root, err := parseDocument(data)
	if err != nil {
		return Declaration{}, err
	}
	d, err := decodeDeclaration(root)
	if err != nil {
		return Declaration{}, fmt.Errorf("schema: %w", err)
	}
	return d, nil
}

func parseDocument(data []byte) (table, error) {
	root := table{}
	current := root
	declared := map[string]bool{"": true}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line, err := withoutComment(scanner.Text())
		if err != nil {
			return nil, lineError(lineNo, err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			path, array, err := parseHeader(line)
			if err != nil {
				return nil, lineError(lineNo, err)
			}
			current, err = enterTable(root, path, array, declared)
			if err != nil {
				return nil, lineError(lineNo, err)
			}
			continue
		}
		key, raw, err := splitAssignment(line)
		if err != nil {
			return nil, lineError(lineNo, err)
		}
		if _, exists := current[key]; exists {
			return nil, lineError(lineNo, fmt.Errorf("duplicate key %q", key))
		}
		value, err := parseScalarOrArray(raw)
		if err != nil {
			return nil, lineError(lineNo, fmt.Errorf("%s: %w", key, err))
		}
		current[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read declaration: %w", err)
	}
	return root, nil
}

func withoutComment(line string) (string, error) {
	inString := false
	escaped := false
	for i, r := range line {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if r == '\\' {
				escaped = true
			} else if r == '"' {
				inString = false
			}
			continue
		}
		if r == '"' {
			inString = true
		} else if r == '#' {
			return line[:i], nil
		}
	}
	if inString {
		return "", fmt.Errorf("unterminated string")
	}
	return line, nil
}

func parseHeader(line string) ([]string, bool, error) {
	array := strings.HasPrefix(line, "[[")
	open, close := "[", "]"
	if array {
		open, close = "[[", "]]"
	}
	if !strings.HasSuffix(line, close) {
		return nil, false, fmt.Errorf("malformed table header")
	}
	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, open), close))
	if body == "" || strings.ContainsAny(body, "[]") {
		return nil, false, fmt.Errorf("malformed table header")
	}
	parts := strings.Split(body, ".")
	for _, part := range parts {
		if !bareKey(part) {
			return nil, false, fmt.Errorf("invalid table name %q", part)
		}
	}
	return parts, array, nil
}

func enterTable(root table, path []string, array bool, declared map[string]bool) (table, error) {
	cur := root
	for i, name := range path {
		last := i == len(path)-1
		full := strings.Join(path[:i+1], ".")
		value, exists := cur[name]
		if last && array {
			if !exists {
				value = []any{}
			}
			items, ok := value.([]any)
			if !ok {
				return nil, fmt.Errorf("%s is already a non-array value", full)
			}
			next := table{}
			cur[name] = append(items, next)
			return next, nil
		}
		if !exists {
			next := table{}
			cur[name] = next
			cur = next
		} else {
			next, ok := value.(table)
			if !ok {
				return nil, fmt.Errorf("%s is already a value or array", full)
			}
			cur = next
		}
		if last {
			if declared[full] {
				return nil, fmt.Errorf("duplicate table %q", full)
			}
			declared[full] = true
		}
	}
	return cur, nil
}

func splitAssignment(line string) (string, string, error) {
	inString, escaped, depth := false, false, 0
	for i, r := range line {
		if inString {
			if escaped {
				escaped = false
			} else if r == '\\' {
				escaped = true
			} else if r == '"' {
				inString = false
			}
			continue
		}
		switch r {
		case '"':
			inString = true
		case '[':
			depth++
		case ']':
			depth--
		case '=':
			if depth == 0 {
				key := strings.TrimSpace(line[:i])
				raw := strings.TrimSpace(line[i+1:])
				if !bareKey(key) || raw == "" {
					return "", "", fmt.Errorf("invalid assignment")
				}
				return key, raw, nil
			}
		}
	}
	return "", "", fmt.Errorf("expected key = value")
}

func bareKey(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

type valueParser struct {
	s string
	i int
}

func parseScalarOrArray(raw string) (any, error) {
	p := &valueParser{s: raw}
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	p.space()
	if p.i != len(p.s) {
		return nil, fmt.Errorf("unexpected trailing material %q", p.s[p.i:])
	}
	return v, nil
}

func (p *valueParser) value() (any, error) {
	p.space()
	if p.i >= len(p.s) {
		return nil, fmt.Errorf("missing value")
	}
	switch p.s[p.i] {
	case '"':
		return p.stringValue()
	case '[':
		return p.arrayValue()
	default:
		start := p.i
		for p.i < len(p.s) && !strings.ContainsRune(" ,]", rune(p.s[p.i])) {
			p.i++
		}
		token := p.s[start:p.i]
		if token == "true" {
			return true, nil
		}
		if token == "false" {
			return false, nil
		}
		return parseInteger(token)
	}
}

func (p *valueParser) stringValue() (string, error) {
	start := p.i
	p.i++
	escaped := false
	for p.i < len(p.s) {
		c := p.s[p.i]
		p.i++
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
		} else if c == '"' {
			var value string
			if err := json.Unmarshal([]byte(p.s[start:p.i]), &value); err != nil {
				return "", fmt.Errorf("invalid string: %w", err)
			}
			return value, nil
		}
	}
	return "", fmt.Errorf("unterminated string")
}

func (p *valueParser) arrayValue() ([]any, error) {
	p.i++
	p.space()
	items := []any{}
	var kind string
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return items, nil
	}
	for {
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		if _, nested := v.([]any); nested {
			return nil, fmt.Errorf("nested arrays are not supported")
		}
		thisKind := fmt.Sprintf("%T", v)
		if kind != "" && thisKind != kind {
			return nil, fmt.Errorf("array elements must have one type")
		}
		kind = thisKind
		items = append(items, v)
		p.space()
		if p.i >= len(p.s) {
			return nil, fmt.Errorf("unterminated array")
		}
		if p.s[p.i] == ']' {
			p.i++
			return items, nil
		}
		if p.s[p.i] != ',' {
			return nil, fmt.Errorf("expected comma in array")
		}
		p.i++
		p.space()
		if p.i < len(p.s) && p.s[p.i] == ']' {
			p.i++
			return items, nil
		}
	}
}

func (p *valueParser) space() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
}

func parseInteger(token string) (int64, error) {
	if token == "" {
		return 0, fmt.Errorf("missing value")
	}
	digits := token
	if digits[0] == '+' || digits[0] == '-' {
		digits = digits[1:]
	}
	if digits == "" || digits[0] == '_' || digits[len(digits)-1] == '_' || strings.Contains(digits, "__") {
		return 0, fmt.Errorf("unsupported value %q", token)
	}
	for _, r := range digits {
		if !(r >= '0' && r <= '9' || r == '_') {
			return 0, fmt.Errorf("unsupported value %q", token)
		}
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(token, "_", ""), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", token)
	}
	return n, nil
}

func lineError(line int, err error) error {
	return fmt.Errorf("line %d: %w", line, err)
}
