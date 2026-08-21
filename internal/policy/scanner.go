package policy

import "strings"

// findUnsafeComment recognizes MySQL executable comments (/*! ... */) and
// optimizer hints (/*+ ... */) only while outside quoted strings, identifiers,
// and ordinary comments. Vitess expands executable comments according to the
// configured MySQL version, so rejecting them before parsing prevents their
// contents from changing the apparent AST.
func findUnsafeComment(sql string) (kind string, found bool) {
	if kind, found := scanUnsafeComment(sql, true); found {
		return kind, true
	}
	// NO_BACKSLASH_ESCAPES changes where a quoted string ends. Scan both SQL
	// modes because parser policy must remain safe even when a datasource's
	// session sql_mode differs from Vitess's lexer assumptions.
	return scanUnsafeComment(sql, false)
}

func scanUnsafeComment(sql string, backslashEscapes bool) (kind string, found bool) {
	const (
		stateSQL = iota
		stateSingleQuote
		stateDoubleQuote
		stateBacktick
		stateLineComment
		stateBlockComment
	)

	state := stateSQL
	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		switch state {
		case stateSQL:
			switch ch {
			case '\'':
				state = stateSingleQuote
			case '"':
				state = stateDoubleQuote
			case '`':
				state = stateBacktick
			case '#':
				state = stateLineComment
			case '-':
				// MySQL recognizes -- as a comment only when followed by
				// whitespace/control or end-of-input.
				if i+1 < len(sql) && sql[i+1] == '-' &&
					(i+2 == len(sql) || isMySQLSpace(sql[i+2])) {
					state = stateLineComment
					i++
				}
			case '/':
				if i+1 >= len(sql) || sql[i+1] != '*' {
					continue
				}
				if i+2 < len(sql) {
					switch sql[i+2] {
					case '!':
						return "MySQL executable comment", true
					case '+':
						return "optimizer hint", true
					}
				}
				state = stateBlockComment
				i++
			}

		case stateSingleQuote:
			if backslashEscapes && ch == '\\' && i+1 < len(sql) {
				i++
				continue
			}
			if ch == '\'' {
				if i+1 < len(sql) && sql[i+1] == '\'' {
					i++ // MySQL's doubled quote escape.
				} else {
					state = stateSQL
				}
			}

		case stateDoubleQuote:
			if backslashEscapes && ch == '\\' && i+1 < len(sql) {
				i++
				continue
			}
			if ch == '"' {
				if i+1 < len(sql) && sql[i+1] == '"' {
					i++
				} else {
					state = stateSQL
				}
			}

		case stateBacktick:
			if ch == '`' {
				if i+1 < len(sql) && sql[i+1] == '`' {
					i++
				} else {
					state = stateSQL
				}
			}

		case stateLineComment:
			if ch == '\n' || ch == '\r' {
				state = stateSQL
			}

		case stateBlockComment:
			if ch == '*' && i+1 < len(sql) && sql[i+1] == '/' {
				state = stateSQL
				i++
			}
		}
	}
	return "", false
}

// findAmbiguousBuiltinCall rejects a raw spelling that MySQL can resolve as a
// stored-function call while Vitess represents it as the corresponding
// built-in. MySQL's default lexer recognizes names in the SYM_FN class as
// built-ins only when "(" follows immediately. With a separating whitespace or
// comment, the special grammar token becomes an identifier and function
// resolution can reach a same-named routine in the current database.
//
// The AST has no token-span or whitespace metadata, so this distinction must
// be checked before parsing. This scanner is deliberately SQL-aware: strings,
// quoted identifiers, and comments are consumed as lexical units rather than
// searched with a regular expression. As with executable-comment detection,
// both backslash modes are scanned because sql_mode belongs to the datasource
// session and must not weaken the authorization boundary.
func findAmbiguousBuiltinCall(sql string) (name string, found bool) {
	if name, found := scanAmbiguousBuiltinCall(sql, true); found {
		return name, true
	}
	return scanAmbiguousBuiltinCall(sql, false)
}

func scanAmbiguousBuiltinCall(sql string, backslashEscapes bool) (name string, found bool) {
	for i := 0; i < len(sql); {
		switch ch := sql[i]; {
		case isMySQLSpace(ch):
			i++
		case ch == '#':
			i = skipMySQLLineComment(sql, i+1)
		case ch == '-' && startsMySQLDashComment(sql, i):
			i = skipMySQLLineComment(sql, i+2)
		case ch == '/' && i+1 < len(sql) && sql[i+1] == '*':
			i = skipMySQLBlockComment(sql, i+2)
		case ch == '\'' || ch == '"':
			i = skipMySQLQuotedString(sql, i, ch, backslashEscapes)
		case ch == '`':
			identifier, next, closed := readBacktickIdentifier(sql, i)
			if !closed {
				return "", false
			}
			lowerName := strings.ToLower(identifier)
			if isWhitespaceSensitiveBuiltinFunction(lowerName) {
				callStart, _ := skipMySQLTrivia(sql, next)
				// A quoted function name is always an identifier, even without a
				// separating space, and can therefore only name a stored routine.
				if callStart < len(sql) && sql[callStart] == '(' {
					return lowerName, true
				}
			}
			i = next
		case isMySQLIdentifierStart(ch):
			start := i
			for i < len(sql) && isMySQLIdentifierPart(sql[i]) {
				i++
			}
			lowerName := strings.ToLower(sql[start:i])
			if !isWhitespaceSensitiveBuiltinFunction(lowerName) {
				continue
			}
			callStart, separated := skipMySQLTrivia(sql, i)
			if separated && callStart < len(sql) && sql[callStart] == '(' {
				return lowerName, true
			}
		default:
			i++
		}
	}
	return "", false
}

// skipMySQLTrivia returns the first byte after whitespace and comments. The
// boolean records whether at least one lexical separator was present; that is
// the security-relevant difference between ADDDATE(...) and ADDDATE (...).
func skipMySQLTrivia(sql string, start int) (next int, separated bool) {
	i := start
	for i < len(sql) {
		switch {
		case isMySQLSpace(sql[i]):
			separated = true
			i++
		case sql[i] == '#':
			separated = true
			i = skipMySQLLineComment(sql, i+1)
		case sql[i] == '-' && startsMySQLDashComment(sql, i):
			separated = true
			i = skipMySQLLineComment(sql, i+2)
		case sql[i] == '/' && i+1 < len(sql) && sql[i+1] == '*':
			separated = true
			i = skipMySQLBlockComment(sql, i+2)
		default:
			return i, separated
		}
	}
	return i, separated
}

func skipMySQLLineComment(sql string, start int) int {
	for start < len(sql) && sql[start] != '\n' && sql[start] != '\r' {
		start++
	}
	return start
}

func skipMySQLBlockComment(sql string, start int) int {
	for start+1 < len(sql) {
		if sql[start] == '*' && sql[start+1] == '/' {
			return start + 2
		}
		start++
	}
	return len(sql)
}

func skipMySQLQuotedString(sql string, start int, quote byte, backslashEscapes bool) int {
	for i := start + 1; i < len(sql); i++ {
		if backslashEscapes && sql[i] == '\\' && i+1 < len(sql) {
			i++
			continue
		}
		if sql[i] != quote {
			continue
		}
		if i+1 < len(sql) && sql[i+1] == quote {
			i++
			continue
		}
		return i + 1
	}
	return len(sql)
}

func readBacktickIdentifier(sql string, start int) (identifier string, next int, closed bool) {
	var value strings.Builder
	for i := start + 1; i < len(sql); i++ {
		if sql[i] != '`' {
			value.WriteByte(sql[i])
			continue
		}
		if i+1 < len(sql) && sql[i+1] == '`' {
			value.WriteByte('`')
			i++
			continue
		}
		return value.String(), i + 1, true
	}
	return "", len(sql), false
}

func startsMySQLDashComment(sql string, start int) bool {
	return start+1 < len(sql) && sql[start+1] == '-' &&
		(start+2 == len(sql) || isMySQLSpace(sql[start+2]))
}

func isMySQLIdentifierStart(ch byte) bool {
	return ch == '_' || ch == '$' || ch >= 0x80 ||
		ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z'
}

func isMySQLIdentifierPart(ch byte) bool {
	return isMySQLIdentifierStart(ch) || ch >= '0' && ch <= '9'
}

func isMySQLSpace(ch byte) bool {
	switch ch {
	case ' ', '\t', '\n', '\r', '\v', '\f', 0:
		return true
	default:
		return false
	}
}
