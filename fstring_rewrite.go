package taipan

import (
	"strings"

	"github.com/lox/taipan/py"
)

const fStringPlaceholder = "\x00"

// rewriteFStrings converts a practical subset of Python f-strings into old
// %-formatting so the 3.4-era parser can execute common agent code.
func rewriteFStrings(source string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(source); {
		if isFStringStart(source, i) {
			rewritten, next, err := rewriteFStringAt(source, i)
			if err != nil {
				return "", err
			}
			out.WriteString(rewritten)
			i = next
			continue
		}

		ch := source[i]
		if ch == '#' {
			lineEnd := i + 1
			for lineEnd < len(source) && source[lineEnd] != '\n' {
				lineEnd++
			}
			out.WriteString(source[i:lineEnd])
			i = lineEnd
			continue
		}

		if ch == '\'' || ch == '"' {
			end, err := scanStringLiteral(source, i)
			if err != nil {
				return "", err
			}
			out.WriteString(source[i:end])
			i = end
			continue
		}

		out.WriteByte(ch)
		i++
	}
	return out.String(), nil
}

func isFStringStart(source string, i int) bool {
	if i < 0 || i+1 >= len(source) {
		return false
	}
	if source[i] != 'f' && source[i] != 'F' {
		return false
	}
	if source[i+1] != '\'' && source[i+1] != '"' {
		return false
	}
	if i > 0 && isIdentifierByte(source[i-1]) {
		return false
	}
	return true
}

func rewriteFStringAt(source string, start int) (string, int, error) {
	quote := source[start+1]
	quoteLen := 1
	if start+3 < len(source) && source[start+2] == quote && source[start+3] == quote {
		quoteLen = 3
	}

	litStart := start + 1
	litEnd, err := scanStringLiteral(source, litStart)
	if err != nil {
		return "", 0, err
	}

	contentStart := litStart + quoteLen
	contentEnd := litEnd - quoteLen
	if contentStart > contentEnd {
		return "", 0, py.ExceptionNewf(py.SyntaxError, "invalid f-string")
	}

	content := source[contentStart:contentEnd]
	template, exprs, err := parseFStringContent(content)
	if err != nil {
		return "", 0, err
	}

	template = strings.ReplaceAll(template, "%", "%%")
	template = strings.ReplaceAll(template, fStringPlaceholder, "%s")
	quoteText := strings.Repeat(string(quote), quoteLen)
	stringLiteral := quoteText + template + quoteText

	if len(exprs) == 0 {
		return stringLiteral, litEnd, nil
	}
	if len(exprs) == 1 {
		return stringLiteral + " % (" + exprs[0] + ",)", litEnd, nil
	}
	return stringLiteral + " % (" + strings.Join(exprs, ", ") + ")", litEnd, nil
}

func parseFStringContent(content string) (string, []string, error) {
	var (
		template strings.Builder
		exprs    []string
	)

	for i := 0; i < len(content); {
		ch := content[i]
		switch ch {
		case '\\':
			if i+1 < len(content) {
				template.WriteByte(content[i])
				template.WriteByte(content[i+1])
				i += 2
				continue
			}
			return "", nil, py.ExceptionNewf(py.SyntaxError, "unterminated escape in f-string")
		case '{':
			if i+1 < len(content) && content[i+1] == '{' {
				template.WriteByte('{')
				i += 2
				continue
			}
			expr, next, err := parseFStringExpr(content, i+1)
			if err != nil {
				return "", nil, err
			}
			exprs = append(exprs, expr)
			template.WriteString(fStringPlaceholder)
			i = next
			continue
		case '}':
			if i+1 < len(content) && content[i+1] == '}' {
				template.WriteByte('}')
				i += 2
				continue
			}
			return "", nil, py.ExceptionNewf(py.SyntaxError, "single '}' is not allowed in f-string")
		default:
			template.WriteByte(ch)
			i++
		}
	}

	return template.String(), exprs, nil
}

func parseFStringExpr(content string, start int) (string, int, error) {
	depth := 0
	var expr strings.Builder

	for i := start; i < len(content); {
		ch := content[i]
		switch ch {
		case '\\':
			if i+1 < len(content) {
				expr.WriteByte(content[i])
				expr.WriteByte(content[i+1])
				i += 2
				continue
			}
			return "", 0, py.ExceptionNewf(py.SyntaxError, "unterminated escape in f-string expression")
		case '\'', '"':
			end, err := scanStringLiteral(content, i)
			if err != nil {
				return "", 0, err
			}
			expr.WriteString(content[i:end])
			i = end
			continue
		case '{':
			depth++
			expr.WriteByte(ch)
			i++
			continue
		case '}':
			if depth == 0 {
				parsed := strings.TrimSpace(expr.String())
				if parsed == "" {
					return "", 0, py.ExceptionNewf(py.SyntaxError, "empty expression not allowed in f-string")
				}
				return parsed, i + 1, nil
			}
			depth--
			expr.WriteByte(ch)
			i++
			continue
		case ':', '!':
			if depth == 0 {
				return "", 0, py.ExceptionNewf(py.SyntaxError, "f-string format specifiers are not supported yet")
			}
		}

		expr.WriteByte(ch)
		i++
	}

	return "", 0, py.ExceptionNewf(py.SyntaxError, "unterminated f-string expression")
}

func scanStringLiteral(source string, start int) (int, error) {
	if start >= len(source) {
		return 0, py.ExceptionNewf(py.SyntaxError, "invalid string literal")
	}
	quote := source[start]
	if quote != '\'' && quote != '"' {
		return 0, py.ExceptionNewf(py.SyntaxError, "invalid string literal")
	}

	quoteLen := 1
	if start+2 < len(source) && source[start+1] == quote && source[start+2] == quote {
		quoteLen = 3
	}

	for i := start + quoteLen; i < len(source); {
		if source[i] == '\\' {
			i += 2
			continue
		}

		if quoteLen == 1 {
			if source[i] == quote {
				return i + 1, nil
			}
			i++
			continue
		}

		if i+2 < len(source) && source[i] == quote && source[i+1] == quote && source[i+2] == quote {
			return i + 3, nil
		}
		i++
	}

	return 0, py.ExceptionNewf(py.SyntaxError, "unterminated string literal")
}

func isIdentifierByte(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_'
}
