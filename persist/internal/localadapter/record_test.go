package localadapter

import (
	"encoding/binary"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestRecordTimestampRoundTrip(t *testing.T) {
	for _, expires := range []time.Time{time.Unix(-1, 999999999), time.Unix(0, 0), time.Unix(1<<40, 999999999)} {
		data, err := encodeRecord("key", []byte("value"), expires, true)
		if err != nil {
			t.Fatal(err)
		}
		binary.BigEndian.PutUint64(data[17:25], 1)
		got, err := decodeRecord(data)
		if err != nil || !got.expires || !got.expiresAt.Equal(expires) || got.key != "key" || string(got.payload) != "value" {
			t.Fatalf("timestamp %v: %+v, %v", expires, got, err)
		}
	}
}

func TestMalformedRecords(t *testing.T) {
	data, err := encodeRecord("key", []byte("value"), time.Time{}, false)
	if err != nil {
		t.Fatal(err)
	}
	data[24] = 1
	for _, mutate := range []func([]byte){func(b []byte) { b[0] = 'X' }, func(b []byte) { b[4] = 2 }, func(b []byte) { b[24] = 0 }, func(b []byte) { b[25] = 255 }, func(b []byte) { b[16] = 1 }} {
		broken := slices.Clone(data)
		mutate(broken)
		if _, err := decodeRecord(broken); !errors.Is(err, ErrCorruptEntry) {
			t.Fatalf("invalid record: %v", err)
		}
	}
}
