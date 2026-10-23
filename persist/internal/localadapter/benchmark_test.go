package localadapter

import (
	"testing"
	"time"
)

func BenchmarkEncodeRecord(b *testing.B) {
	payload := make([]byte, 1024)
	deadline := time.Unix(100, 0)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := encodeRecord("gac2:4:http:key", payload, deadline, true); err != nil {
			b.Fatal(err)
		}
	}
}
