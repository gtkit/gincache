package localadapter

import (
	"encoding/binary"
	"strings"
	"time"
	"unicode/utf8"
)

const recordHeaderSize = 29

// RecordKey 从内部记录提取完整键，供不保留字符串键的原生迭代器使用。
func RecordKey(data []byte) (string, error) {
	r, err := decodeRecord(data)
	return r.key, err
}

type record struct {
	key       string
	payload   []byte
	expiresAt time.Time
	sequence  uint64
	expires   bool
}

func validNamespace(namespace string) bool {
	return len(namespace) > 0 && len(namespace) <= 128 && utf8.ValidString(namespace) && !strings.ContainsRune(namespace, 0)
}

func encodeRecord(key string, value []byte, expiresAt time.Time, expires bool) ([]byte, error) {
	keyLen := uint64(len(key))
	if keyLen > uint64(^uint32(0)) || len(value) > int(^uint(0)>>1)-recordHeaderSize-len(key) {
		return nil, ErrInvalidArgument
	}
	data := make([]byte, recordHeaderSize+len(key)+len(value))
	copy(data, "GAC2")
	if expires {
		data[4] = 1
		binary.BigEndian.PutUint64(data[5:13], uint64(expiresAt.Unix()))        //nolint:gosec // G115: 保留有符号 Unix 秒的 64 位补码，读取时恢复。
		binary.BigEndian.PutUint32(data[13:17], uint32(expiresAt.Nanosecond())) //nolint:gosec // G115: time.Time.Nanosecond 的范围为 [0, 999999999]。
	}
	binary.BigEndian.PutUint32(data[25:29], uint32(keyLen))
	copy(data[29:], key)
	copy(data[29+len(key):], value)
	return data, nil
}

func decodeRecord(data []byte) (record, error) {
	if len(data) < recordHeaderSize || string(data[:4]) != "GAC2" || data[4] > 1 {
		return record{}, ErrCorruptEntry
	}
	seconds := int64(binary.BigEndian.Uint64(data[5:13])) //nolint:gosec // G115: 按二进制补码恢复有符号 Unix 秒，负数是合法时间。
	nanos := binary.BigEndian.Uint32(data[13:17])
	sequence := binary.BigEndian.Uint64(data[17:25])
	keyLen := uint64(binary.BigEndian.Uint32(data[25:29]))
	if nanos >= 1e9 || sequence == 0 || keyLen > uint64(len(data))-recordHeaderSize ||
		(data[4] == 0 && (seconds != 0 || nanos != 0)) {
		return record{}, ErrCorruptEntry
	}
	end := recordHeaderSize + keyLen
	r := record{key: string(data[recordHeaderSize:end]), payload: data[end:], sequence: sequence, expires: data[4] == 1}
	if r.expires {
		r.expiresAt = time.Unix(seconds, int64(nanos))
	}
	return r, nil
}
