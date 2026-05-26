// Package canonjson produces a byte-for-byte canonical JSON encoding that
// matches Python's
//
//	json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=True)
//
// applied after Pydantic's exclude_none=True filter. This encoder exists for
// one reason: the protocol HMAC signature is computed over exactly those
// bytes, and the Go agent's auth check must regenerate the same bytes or
// every signed request fails.
//
// Differences vs Go's encoding/json that this package fixes:
//   - object keys are sorted lexicographically by Unicode code point
//   - nil entries inside maps are dropped (matches Pydantic exclude_none)
//   - non-ASCII characters are escaped as \uXXXX (Python ensure_ascii=True
//     default; Go's encoder emits raw UTF-8)
//   - HTML escapes (<, >, &) are NOT applied (Go's encoder escapes them by
//     default; Python does not)
//   - whole-number floats keep a ".0" suffix (1.0 → "1.0", not "1")
package canonjson

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"unicode/utf8"
)

// Encode returns the canonical JSON encoding of v with no whitespace between
// members or key/value pairs — matching Python's
//
//	json.dumps(v, sort_keys=True, separators=(",", ":"), ensure_ascii=True)
//
// This form is used for the partial-keys body hash.
//
// Supported types: nil, bool, all sized int/uint/float types, string,
// []any, map[string]any. Structs are rejected; convert via json.Marshal+Unmarshal
// first if you have one.
func Encode(v any) ([]byte, error) {
	return encodeWith(v, ",", ":")
}

// EncodeForSignature returns the canonical JSON encoding of v with Python's
// default separators (", " and ": ") — matching
//
//	json.dumps(v, sort_keys=True, ensure_ascii=True)   # default separators
//
// This form is used for the HMAC signature path (sign_action_request line 40,
// which omits the separators argument). The sign payload is built as
// `v0:` + EncodeForSignature(body), HMAC'd, and prefixed `v0=`.
func EncodeForSignature(v any) ([]byte, error) {
	return encodeWith(v, ", ", ": ")
}

func encodeWith(v any, itemSep, kvSep string) ([]byte, error) {
	var buf bytes.Buffer
	enc := encoder{itemSep: itemSep, kvSep: kvSep}
	if err := enc.encode(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type encoder struct {
	itemSep string
	kvSep   string
}

func (e *encoder) encode(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeString(buf, x)
	case int:
		buf.WriteString(strconv.FormatInt(int64(x), 10))
	case int8:
		buf.WriteString(strconv.FormatInt(int64(x), 10))
	case int16:
		buf.WriteString(strconv.FormatInt(int64(x), 10))
	case int32:
		buf.WriteString(strconv.FormatInt(int64(x), 10))
	case int64:
		buf.WriteString(strconv.FormatInt(x, 10))
	case uint:
		buf.WriteString(strconv.FormatUint(uint64(x), 10))
	case uint8:
		buf.WriteString(strconv.FormatUint(uint64(x), 10))
	case uint16:
		buf.WriteString(strconv.FormatUint(uint64(x), 10))
	case uint32:
		buf.WriteString(strconv.FormatUint(uint64(x), 10))
	case uint64:
		buf.WriteString(strconv.FormatUint(x, 10))
	case float32:
		return writeFloat(buf, float64(x))
	case float64:
		return writeFloat(buf, x)
	case []any:
		return e.writeArray(buf, x)
	case map[string]any:
		return e.writeObject(buf, x)
	default:
		return fmt.Errorf("canonjson: unsupported type %T", v)
	}
	return nil
}

func (e *encoder) writeArray(buf *bytes.Buffer, a []any) error {
	buf.WriteByte('[')
	for i, item := range a {
		if i > 0 {
			buf.WriteString(e.itemSep)
		}
		if err := e.encode(buf, item); err != nil {
			return err
		}
	}
	buf.WriteByte(']')
	return nil
}

func (e *encoder) writeObject(buf *bytes.Buffer, m map[string]any) error {
	keys := make([]string, 0, len(m))
	for k, v := range m {
		if v == nil {
			continue // exclude_none
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteString(e.itemSep)
		}
		writeString(buf, k)
		buf.WriteString(e.kvSep)
		if err := e.encode(buf, m[k]); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

func writeFloat(buf *bytes.Buffer, f float64) error {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return errors.New("canonjson: NaN/Inf not representable in strict JSON")
	}
	// Python's json.dumps uses float.__repr__, which is the shortest string
	// that round-trips. Go's strconv.FormatFloat(f, 'g', -1, 64) is also
	// shortest-round-trip but drops the trailing ".0" for whole numbers.
	// Add it back when there is no decimal point and no exponent.
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !bytes.ContainsAny([]byte(s), ".eE") {
		s += ".0"
	}
	buf.WriteString(s)
	return nil
}

// writeString emits a JSON string literal matching Python's json.dumps with
// ensure_ascii=True.
func writeString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '"':
			buf.WriteString(`\"`)
		case r == '\\':
			buf.WriteString(`\\`)
		case r == '\b':
			buf.WriteString(`\b`)
		case r == '\f':
			buf.WriteString(`\f`)
		case r == '\n':
			buf.WriteString(`\n`)
		case r == '\r':
			buf.WriteString(`\r`)
		case r == '\t':
			buf.WriteString(`\t`)
		case r < 0x20:
			fmt.Fprintf(buf, `\u%04x`, r)
		case r < 0x7f:
			buf.WriteByte(byte(r))
		case r >= 0x10000:
			// Encode as UTF-16 surrogate pair.
			r -= 0x10000
			hi := 0xd800 + (r >> 10)
			lo := 0xdc00 + (r & 0x3ff)
			fmt.Fprintf(buf, `\u%04x\u%04x`, hi, lo)
		default:
			fmt.Fprintf(buf, `\u%04x`, r)
		}
		i += size
	}
	buf.WriteByte('"')
}
