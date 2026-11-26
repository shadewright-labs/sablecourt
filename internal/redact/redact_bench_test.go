package redact

import (
	"strings"
	"testing"
)

func BenchmarkTextOperatorTokens(b *testing.B) {
	input := strings.Join([]string{
		"task=sc_task_0123456789abcdef0123456789abcdef",
		"operator=op_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"ordinary log line without credentials",
	}, "\n")
	b.ReportAllocs()
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	for range b.N {
		Text(input)
	}
}
