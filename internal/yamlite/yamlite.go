// Package yamlite reads the subset of YAML a configuration file needs -
// nested mappings by indentation, block and flow sequences, quoted and plain
// scalars, comments - and decodes it into a struct through its json tags, so
// one set of tags serves both node.yaml and JSON. It is deliberately small;
// anchors, multi-line scalars and flow mappings are not supported.
package yamlite

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Unmarshal decodes YAML (or JSON: a document starting with "{") into v.
func Unmarshal(data []byte, v any) error {
	text := strings.TrimSpace(string(data))
	if strings.HasPrefix(text, "{") {
		return json.Unmarshal([]byte(text), v)
	}
	tree, err := Parse(data)
	if err != nil {
		return err
	}
	b, err := json.Marshal(tree)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Parse returns the document as map[string]any / []any / scalars.
func Parse(data []byte) (any, error) {
	var lines []line
	for i, raw := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		text := stripComment(raw)
		if strings.TrimSpace(text) == "" || strings.TrimSpace(text) == "---" {
			continue
		}
		if strings.Contains(text, "\t") && strings.TrimLeft(text, " ") != strings.TrimLeft(text, " \t") {
			return nil, fmt.Errorf("yaml line %d: tabs are not allowed for indentation", i+1)
		}
		indent := len(text) - len(strings.TrimLeft(text, " "))
		lines = append(lines, line{no: i + 1, indent: indent, text: strings.TrimSpace(text)})
	}
	if len(lines) == 0 {
		return map[string]any{}, nil
	}
	p := &parser{lines: lines}
	v, err := p.block(lines[0].indent)
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.lines) {
		return nil, fmt.Errorf("yaml line %d: unexpected indentation", p.lines[p.pos].no)
	}
	return v, nil
}

type line struct {
	no, indent int
	text       string
}

type parser struct {
	lines []line
	pos   int
}

// block parses the mapping or sequence whose items sit at the given indent.
func (p *parser) block(indent int) (any, error) {
	if strings.HasPrefix(p.lines[p.pos].text, "- ") || p.lines[p.pos].text == "-" {
		return p.sequence(indent)
	}
	return p.mapping(indent)
}

func (p *parser) mapping(indent int) (map[string]any, error) {
	out := map[string]any{}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent < indent {
			return out, nil
		}
		if l.indent > indent {
			return nil, fmt.Errorf("yaml line %d: unexpected indentation", l.no)
		}
		if strings.HasPrefix(l.text, "- ") {
			return nil, fmt.Errorf("yaml line %d: sequence item where a key was expected", l.no)
		}
		key, rest, ok := splitKey(l.text)
		if !ok {
			return nil, fmt.Errorf("yaml line %d: expected \"key: value\"", l.no)
		}
		p.pos++
		if rest != "" {
			v, err := scalar(rest, l.no)
			if err != nil {
				return nil, err
			}
			out[key] = v
			continue
		}
		// A key with nothing after the colon introduces a nested block, or is
		// empty (null) when the next line is not indented further.
		if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
			v, err := p.block(p.lines[p.pos].indent)
			if err != nil {
				return nil, err
			}
			out[key] = v
		} else if p.pos < len(p.lines) && p.lines[p.pos].indent == indent && strings.HasPrefix(p.lines[p.pos].text, "- ") {
			// "key:\n- a\n- b" - a sequence at the same indent as its key.
			v, err := p.sequence(indent)
			if err != nil {
				return nil, err
			}
			out[key] = v
		} else {
			out[key] = nil
		}
	}
	return out, nil
}

func (p *parser) sequence(indent int) ([]any, error) {
	out := []any{}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent < indent {
			return out, nil
		}
		if l.indent > indent || !(strings.HasPrefix(l.text, "- ") || l.text == "-") {
			return nil, fmt.Errorf("yaml line %d: expected a \"- \" item", l.no)
		}
		item := strings.TrimSpace(strings.TrimPrefix(l.text, "-"))
		p.pos++
		switch {
		case item == "":
			if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
				v, err := p.block(p.lines[p.pos].indent)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			} else {
				out = append(out, nil)
			}
		case isKeyValue(item):
			// "- key: value" starts an inline mapping whose further keys are
			// indented past the dash.
			key, rest, _ := splitKey(item)
			m := map[string]any{}
			if rest != "" {
				v, err := scalar(rest, l.no)
				if err != nil {
					return nil, err
				}
				m[key] = v
			}
			inner := indent + 2
			if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
				inner = p.lines[p.pos].indent
				more, err := p.mapping(inner)
				if err != nil {
					return nil, err
				}
				for k, v := range more {
					m[k] = v
				}
			}
			out = append(out, m)
		default:
			v, err := scalar(item, l.no)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
	}
	return out, nil
}

// splitKey separates "key: value" at the first ": " (or trailing ":") outside
// quotes.
func splitKey(s string) (key, rest string, ok bool) {
	inQuote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote != 0 {
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			inQuote = c
			continue
		}
		if c == ':' && (i+1 == len(s) || s[i+1] == ' ') {
			key = strings.TrimSpace(s[:i])
			if k, err := unquote(key); err == nil {
				key = k
			}
			return key, strings.TrimSpace(s[i+1:]), key != ""
		}
	}
	return "", "", false
}

// isKeyValue reports whether an item is "key: value" (a colon inside quotes
// does not count, so "- 'a: b'" is a scalar).
func isKeyValue(s string) bool {
	_, _, ok := splitKey(s)
	return ok
}

// scalar turns a value token into a Go value: quoted strings stay strings;
// plain tokens become bool, int, float or null when they look like one; a
// flow sequence [a, b] becomes a list of scalars.
func scalar(s string, no int) (any, error) {
	if strings.HasPrefix(s, "[") {
		if !strings.HasSuffix(s, "]") {
			return nil, fmt.Errorf("yaml line %d: unterminated [ ]", no)
		}
		inner := strings.TrimSpace(s[1 : len(s)-1])
		out := []any{}
		if inner == "" {
			return out, nil
		}
		for _, part := range splitFlow(inner) {
			v, err := scalar(strings.TrimSpace(part), no)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	if strings.HasPrefix(s, "{") {
		return nil, fmt.Errorf("yaml line %d: flow mappings { } are not supported; use indented keys", no)
	}
	if strings.HasPrefix(s, "\"") || strings.HasPrefix(s, "'") {
		return unquote(s)
	}
	switch s {
	case "null", "~":
		return nil, nil
	case "true", "yes", "on":
		return true, nil
	case "false", "no", "off":
		return false, nil
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && !strings.HasPrefix(s, "0x") {
		return f, nil
	}
	return s, nil
}

func unquote(s string) (string, error) {
	if len(s) < 2 {
		return "", fmt.Errorf("bad quoted string %q", s)
	}
	switch s[0] {
	case '"':
		if s[len(s)-1] != '"' {
			return "", fmt.Errorf("unterminated string %s", s)
		}
		return strconv.Unquote(s)
	case '\'':
		if s[len(s)-1] != '\'' {
			return "", fmt.Errorf("unterminated string %s", s)
		}
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), nil
	}
	return "", fmt.Errorf("not quoted: %s", s)
}

// splitFlow splits "a, 'b, c', d" on commas outside quotes.
func splitFlow(s string) []string {
	var out []string
	start, inQuote := 0, byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote != 0 {
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			inQuote = c
		} else if c == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// stripComment removes a "# comment" outside quotes.
func stripComment(s string) string {
	inQuote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote != 0 {
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			inQuote = c
		} else if c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t') {
			return s[:i]
		}
	}
	return s
}
