// Package schemafilter implements the shared schema authorization semantics.
// Exact names and glob patterns are intentionally case-sensitive because
// MySQL database-name case sensitivity depends on the server host platform.
package schemafilter

import (
	"errors"
	"strings"
	"unicode/utf8"
)

const maxSchemaRunes = 64

// Validate checks that pattern can describe a MySQL schema name. An asterisk
// is the only metacharacter; every other character is matched literally.
func Validate(pattern string) error {
	if pattern == "" {
		return errors.New("schema pattern cannot be empty")
	}
	if !utf8.ValidString(pattern) {
		return errors.New("schema pattern must be valid UTF-8")
	}
	if utf8.RuneCountInString(pattern) > maxSchemaRunes {
		return errors.New("schema pattern cannot exceed 64 characters")
	}
	if strings.ContainsRune(pattern, '\x00') {
		return errors.New("schema pattern cannot contain NUL")
	}
	return nil
}

// Match reports whether name matches the complete glob pattern. The asterisk
// matches zero or more characters; matching is anchored and case-sensitive.
// Invalid patterns and invalid UTF-8 names never match.
func Match(pattern, name string) bool {
	if Validate(pattern) != nil || !utf8.ValidString(name) {
		return false
	}

	patternRunes := []rune(pattern)
	nameRunes := []rune(name)
	patternIndex, nameIndex := 0, 0
	starIndex, starNameIndex := -1, -1

	for nameIndex < len(nameRunes) {
		switch {
		case patternIndex < len(patternRunes) && patternRunes[patternIndex] == '*':
			// Remember the most recent star. If the suffix does not match, the
			// fallback below grows this star by one character and tries again.
			starIndex = patternIndex
			starNameIndex = nameIndex
			patternIndex++
		case patternIndex < len(patternRunes) && patternRunes[patternIndex] == nameRunes[nameIndex]:
			patternIndex++
			nameIndex++
		case starIndex >= 0:
			starNameIndex++
			nameIndex = starNameIndex
			patternIndex = starIndex + 1
		default:
			return false
		}
	}

	// A suffix made only of stars is allowed to consume zero characters.
	for patternIndex < len(patternRunes) && patternRunes[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(patternRunes)
}

// Allows reports whether name is present in exact or matches at least one
// pattern. Exact comparisons use the same case-sensitive semantics as Match.
// Empty exact and pattern lists mean unrestricted access.
func Allows(name string, exact, patterns []string) bool {
	if !Restricted(exact, patterns) {
		return true
	}
	for _, allowed := range exact {
		if name == allowed {
			return true
		}
	}
	for _, pattern := range patterns {
		if Match(pattern, name) {
			return true
		}
	}
	return false
}

// Restricted reports whether either form of schema allow-list is configured.
// When it returns false, callers should allow all schemas visible to MySQL.
func Restricted(exact, patterns []string) bool {
	return len(exact) != 0 || len(patterns) != 0
}

// ToSQLLike converts a validated glob to a MySQL LIKE pattern. It uses '=' as
// the escape character, so callers must append "ESCAPE '='" to the predicate.
// Conversion does not validate; configuration boundaries should call Validate.
func ToSQLLike(pattern string) string {
	var converted strings.Builder
	converted.Grow(len(pattern))
	for _, character := range pattern {
		switch character {
		case '*':
			converted.WriteByte('%')
		case '%', '_', '=':
			converted.WriteByte('=')
			converted.WriteRune(character)
		default:
			converted.WriteRune(character)
		}
	}
	return converted.String()
}

// SQLLike is retained as a concise spelling for callers that do not need to
// emphasize the conversion step. New SQL builders may prefer ToSQLLike.
func SQLLike(pattern string) string { return ToSQLLike(pattern) }
