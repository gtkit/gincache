package pattern

import "testing"

func TestMatcherMatchesRedisStyleGlob(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pattern string
		key     string
		want    bool
	}{
		{name: "star", pattern: "user:*", key: "user:42", want: true},
		{name: "question", pattern: "user:?", key: "user:7", want: true},
		{name: "question no match", pattern: "user:?", key: "user:42", want: false},
		{name: "class", pattern: "user:[12]", key: "user:1", want: true},
		{name: "negated class", pattern: "user:[!3]", key: "user:2", want: true},
		{name: "escaped question", pattern: `query:\?`, key: "query:?", want: true},
		{name: "unterminated class literal", pattern: "query:[", key: "query:[", want: true},
		{name: "trailing escape literal", pattern: `query:\`, key: `query:\`, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matcher, err := Compile(tt.pattern)
			if err != nil {
				t.Fatalf("Compile error: %v", err)
			}
			if got := matcher.Match(tt.key); got != tt.want {
				t.Fatalf("Match(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}
