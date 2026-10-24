package runner

import (
	"errors"
	"strings"
)

var errSQL = errors.New("unsupported or malformed migration SQL")

// statements only locates lexical boundaries. PostgreSQL parses each resulting
// statement again through the extended protocol, which rejects multiple commands.
func statements(sql string) ([]string, error) {
	if len(sql) == 0 || len(sql) > 48*1024 || strings.ContainsRune(sql, 0) {
		return nil, errSQL
	}
	var out []string
	start, first := 0, ""
	var tokens []string
	appendStatement := func(end int) error {
		if first == "" {
			return nil
		}
		switch first {
		case "CREATE", "ALTER", "DROP", "TRUNCATE", "COMMENT", "INSERT", "UPDATE", "DELETE", "MERGE", "SELECT", "WITH":
		default:
			return errSQL
		}
		if first == "CREATE" || first == "ALTER" || first == "DROP" {
			for n, token := range tokens[1:] {
				if token == "CONCURRENTLY" {
					return errSQL
				}
				if n != 0 {
					continue
				}
				switch token {
				case "DATABASE", "TABLESPACE", "SUBSCRIPTION", "SYSTEM", "ROLE", "USER", "EXTENSION", "SERVER":
					return errSQL
				}
			}
		}
		out = append(out, sql[start:end])
		if len(out) > 100 {
			return errSQL
		}
		return nil
	}
	for i := 0; i < len(sql); {
		switch {
		case strings.ContainsRune(" \t\r\n\f", rune(sql[i])):
			i++
		case strings.HasPrefix(sql[i:], "--"):
			for i < len(sql) && sql[i] != '\n' && sql[i] != '\r' {
				i++
			}
		case strings.HasPrefix(sql[i:], "/*"):
			i += 2
			depth := 1
			for i < len(sql) && depth > 0 {
				if strings.HasPrefix(sql[i:], "/*") {
					depth++
					i += 2
				} else if strings.HasPrefix(sql[i:], "*/") {
					depth--
					i += 2
				} else {
					i++
				}
			}
			if depth != 0 {
				return nil, errSQL
			}
		case sql[i] == ';':
			if err := appendStatement(i); err != nil {
				return nil, err
			}
			i++
			start, first = i, ""
			tokens = nil
		case sql[i] == '\'' || sql[i] == '"':
			quote := sql[i]
			escape := quote == '\'' && i > 0 && (sql[i-1] == 'e' || sql[i-1] == 'E') && (i < 2 || !word(sql[i-2]))
			i++
			closed := false
			for i < len(sql) {
				if escape && sql[i] == '\\' {
					i += 2
					continue
				}
				if sql[i] == quote {
					i++
					if i < len(sql) && sql[i] == quote {
						i++
						continue
					}
					closed = true
					break
				}
				i++
			}
			if !closed || first == "" {
				return nil, errSQL
			}
		case sql[i] == '$':
			j := i + 1
			for j < len(sql) && (letter(sql[j]) || (j > i+1 && sql[j] >= '0' && sql[j] <= '9')) {
				j++
			}
			if j >= len(sql) || sql[j] != '$' {
				return nil, errSQL
			}
			delim := sql[i : j+1]
			end := strings.Index(sql[j+1:], delim)
			if end < 0 || first == "" {
				return nil, errSQL
			}
			i = j + 1 + end + len(delim)
		case letter(sql[i]):
			j := i + 1
			for j < len(sql) && word(sql[j]) {
				j++
			}
			if first == "" {
				first = strings.ToUpper(sql[i:j])
			}
			tokens = append(tokens, strings.ToUpper(sql[i:j]))
			i = j
		default:
			if first == "" {
				return nil, errSQL
			}
			i++
		}
	}
	if err := appendStatement(len(sql)); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errSQL
	}
	return out, nil
}

func letter(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b == '_' || b >= 128 }
func word(b byte) bool   { return letter(b) || b >= '0' && b <= '9' || b == '$' }
