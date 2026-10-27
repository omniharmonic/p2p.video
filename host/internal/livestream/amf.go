package livestream

import (
	"encoding/binary"
	"errors"
	"math"
	"sort"
)

// Minimal AMF0 (the RTMP command encoding): enough to issue publish
// commands and read the server's replies.

const (
	amfNumber      = 0x00
	amfBoolean     = 0x01
	amfString      = 0x02
	amfObject      = 0x03
	amfNull        = 0x05
	amfUndefined   = 0x06
	amfECMAArray   = 0x08
	amfObjectEnd   = 0x09
	amfStrictArray = 0x0a
	amfDate        = 0x0b
	amfLongString  = 0x0c
)

var errAMF = errors.New("malformed AMF0")

// amfObj is an AMF0 object with keys written in sorted order (stable bytes).
type amfObj map[string]any

func amfAppend(b []byte, v any) []byte {
	switch x := v.(type) {
	case nil:
		return append(b, amfNull)
	case float64:
		b = append(b, amfNumber)
		return binary.BigEndian.AppendUint64(b, math.Float64bits(x))
	case int:
		return amfAppend(b, float64(x))
	case bool:
		if x {
			return append(b, amfBoolean, 1)
		}
		return append(b, amfBoolean, 0)
	case string:
		if len(x) > 0xffff {
			b = append(b, amfLongString)
			b = binary.BigEndian.AppendUint32(b, uint32(len(x)))
			return append(b, x...)
		}
		b = append(b, amfString)
		return amfAppendKey(b, x)
	case amfObj:
		b = append(b, amfObject)
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b = amfAppendKey(b, k)
			b = amfAppend(b, x[k])
		}
		return append(b, 0, 0, amfObjectEnd)
	}
	panic("amf0: unsupported type")
}

func amfAppendKey(b []byte, k string) []byte {
	b = binary.BigEndian.AppendUint16(b, uint16(len(k)))
	return append(b, k...)
}

// amfEncode concatenates values (an RTMP command body).
func amfEncode(vals ...any) []byte {
	var b []byte
	for _, v := range vals {
		b = amfAppend(b, v)
	}
	return b
}

// amfDecodeAll decodes consecutive values. Objects and ECMA arrays become
// map[string]any; strict arrays []any; null/undefined nil.
func amfDecodeAll(b []byte) ([]any, error) {
	var out []any
	for len(b) > 0 {
		v, n, err := amfDecode(b, 0)
		if err != nil {
			return out, err
		}
		out = append(out, v)
		b = b[n:]
	}
	return out, nil
}

func amfDecode(b []byte, depth int) (any, int, error) {
	if len(b) == 0 || depth > 16 {
		return nil, 0, errAMF
	}
	switch b[0] {
	case amfNumber:
		if len(b) < 9 {
			return nil, 0, errAMF
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b[1:9])), 9, nil
	case amfBoolean:
		if len(b) < 2 {
			return nil, 0, errAMF
		}
		return b[1] != 0, 2, nil
	case amfString:
		s, n, err := amfKey(b[1:])
		return s, n + 1, err
	case amfLongString:
		if len(b) < 5 {
			return nil, 0, errAMF
		}
		l := int(binary.BigEndian.Uint32(b[1:5]))
		if l > len(b)-5 {
			return nil, 0, errAMF
		}
		return string(b[5 : 5+l]), 5 + l, nil
	case amfNull, amfUndefined:
		return nil, 1, nil
	case amfObject:
		m, n, err := amfProps(b[1:], depth)
		return m, n + 1, err
	case amfECMAArray:
		if len(b) < 5 {
			return nil, 0, errAMF
		}
		m, n, err := amfProps(b[5:], depth)
		return m, n + 5, err
	case amfStrictArray:
		if len(b) < 5 {
			return nil, 0, errAMF
		}
		count := int(binary.BigEndian.Uint32(b[1:5]))
		pos := 5
		var arr []any
		for i := 0; i < count; i++ {
			v, n, err := amfDecode(b[pos:], depth+1)
			if err != nil {
				return nil, 0, err
			}
			arr = append(arr, v)
			pos += n
		}
		return arr, pos, nil
	case amfDate:
		if len(b) < 11 {
			return nil, 0, errAMF
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b[1:9])), 11, nil
	}
	return nil, 0, errAMF
}

func amfKey(b []byte) (string, int, error) {
	if len(b) < 2 {
		return "", 0, errAMF
	}
	l := int(binary.BigEndian.Uint16(b))
	if l > len(b)-2 {
		return "", 0, errAMF
	}
	return string(b[2 : 2+l]), 2 + l, nil
}

func amfProps(b []byte, depth int) (map[string]any, int, error) {
	m := map[string]any{}
	pos := 0
	for {
		if len(b)-pos >= 3 && b[pos] == 0 && b[pos+1] == 0 && b[pos+2] == amfObjectEnd {
			return m, pos + 3, nil
		}
		k, n, err := amfKey(b[pos:])
		if err != nil {
			return nil, 0, err
		}
		pos += n
		v, n, err := amfDecode(b[pos:], depth+1)
		if err != nil {
			return nil, 0, err
		}
		m[k] = v
		pos += n
	}
}
