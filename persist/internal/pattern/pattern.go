package pattern

import (
	"fmt"
	"regexp"
	"strings"
)

// Matcher matches cache keys with a Redis-style glob pattern.
type Matcher struct {
	re *regexp.Regexp
}

// Compile compiles a Redis-style glob pattern into a reusable matcher.
// Supported tokens:
// - * matches any sequence
// - ? matches any single character
// - [...] character classes
// - \ escapes a special token
func Compile(glob string) (*Matcher, error) {
	re, err := regexp.Compile(globToRegexp(glob))
	if err != nil {
		return nil, fmt.Errorf("compile pattern %q: %w", glob, err)
	}
	return &Matcher{re: re}, nil
}

// Match reports whether key matches the compiled pattern.
func (m *Matcher) Match(key string) bool {
	return m.re.MatchString(key)
}

func globToRegexp(glob string) string {
	var b strings.Builder
	b.Grow(len(glob) * 2)
	b.WriteByte('^')

	for i := 0; i < len(glob); i++ {
		switch glob[i] {
		case '\\':
			if i+1 >= len(glob) {
				b.WriteString(`\\`)
				continue
			}
			i++
			b.WriteString(regexp.QuoteMeta(string(glob[i])))
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteByte('.')
		case '[':
			end, class := translateCharClass(glob, i)
			if end == i {
				b.WriteString(`\[`)
				continue
			}
			b.WriteString(class)
			i = end
		default:
			b.WriteString(regexp.QuoteMeta(string(glob[i])))
		}
	}

	b.WriteByte('$')
	return b.String()
}

func translateCharClass(glob string, start int) (int, string) {
	i := start + 1
	if i >= len(glob) {
		return start, ""
	}

	var b strings.Builder
	b.WriteByte('[')

	if glob[i] == '!' || glob[i] == '^' {
		b.WriteByte('^')
		i++
	}

	if i < len(glob) && glob[i] == ']' {
		b.WriteString(`\]`)
		i++
	}

	for ; i < len(glob); i++ {
		switch glob[i] {
		case '\\':
			if i+1 >= len(glob) {
				b.WriteString(`\\`)
				continue
			}
			i++
			b.WriteString(regexp.QuoteMeta(string(glob[i])))
		case ']':
			b.WriteByte(']')
			return i, b.String()
		default:
			b.WriteByte(glob[i])
		}
	}

	return start, ""
}
