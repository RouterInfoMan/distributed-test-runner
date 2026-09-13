package pg

import (
	"strings"
)

// SplitStatements cuts a script into statements at top-level semicolons,
// leaving alone semicolons inside 'quoted strings', "quoted identifiers",
// $$dollar-quoted$$ bodies (with or without a tag) and comments. The extended
// protocol runs one statement per Parse, so a schema file needs this.
func SplitStatements(script string) []string {
	var out []string
	var cur strings.Builder
	i := 0
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	for i < len(script) {
		c := script[i]
		switch {
		case strings.HasPrefix(script[i:], "--"): // line comment
			for i < len(script) && script[i] != '\n' {
				i++
			}
		case strings.HasPrefix(script[i:], "/*"): // block comment
			end := strings.Index(script[i+2:], "*/")
			if end < 0 {
				i = len(script)
			} else {
				i += end + 4
			}
		case c == '\'' || c == '"':
			end := closingQuote(script, i, c)
			cur.WriteString(script[i:end])
			i = end
		case c == '$':
			if tag := dollarTag(script[i:]); tag != "" {
				end := strings.Index(script[i+len(tag):], tag)
				if end < 0 {
					end = len(script) - i - len(tag)
				}
				stop := i + len(tag) + end + len(tag)
				if stop > len(script) {
					stop = len(script)
				}
				cur.WriteString(script[i:stop])
				i = stop
			} else {
				cur.WriteByte(c)
				i++
			}
		case c == ';':
			flush()
			i++
		default:
			cur.WriteByte(c)
			i++
		}
	}
	flush()
	return out
}

// closingQuote returns the index just past the quote that closes the one at
// start (” and "" escape a quote of the same kind).
func closingQuote(s string, start int, q byte) int {
	i := start + 1
	for i < len(s) {
		if s[i] == q {
			if i+1 < len(s) && s[i+1] == q {
				i += 2
				continue
			}
			return i + 1
		}
		i++
	}
	return len(s)
}

// dollarTag returns "$$" or "$tag$" when s starts with a dollar-quote opener.
func dollarTag(s string) string {
	if len(s) < 2 || s[0] != '$' {
		return ""
	}
	end := strings.IndexByte(s[1:], '$')
	if end < 0 {
		return ""
	}
	tag := s[1 : 1+end]
	for _, r := range tag {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return ""
		}
	}
	return "$" + tag + "$"
}

// Array renders a []string as a PostgreSQL array literal ({"a","b"}).
func Array(vals []string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, v := range vals {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// ParseArray reads a one-dimensional text array literal back into a []string.
func ParseArray(lit string) []string {
	lit = strings.TrimSpace(lit)
	if len(lit) < 2 || lit[0] != '{' || lit[len(lit)-1] != '}' {
		return nil
	}
	body := lit[1 : len(lit)-1]
	if body == "" {
		return []string{}
	}
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case inQuote && c == '\\' && i+1 < len(body):
			i++
			cur.WriteByte(body[i])
		case c == '"':
			inQuote = !inQuote
		case c == ',' && !inQuote:
			out = append(out, unquotedNull(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	out = append(out, unquotedNull(cur.String()))
	return out
}

func unquotedNull(s string) string {
	if s == "NULL" {
		return ""
	}
	return s
}
