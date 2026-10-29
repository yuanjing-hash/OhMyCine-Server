package resourcecontent

import (
	"bytes"
	"strconv"
)

// value slices point into a bounded private input. Dictionaries retain raw
// bytes and original ordering; duplicate keys are always rejected.
type value struct {
	kind byte
	raw  []byte
	data []byte
	n    int64
	list []*value
	dict []pair
}

type pair struct {
	key string
	v   *value
}

func (v *value) get(key string) *value {
	if v == nil || v.kind != 'd' {
		return nil
	}
	for _, p := range v.dict {
		if p.key == key {
			return p.v
		}
	}
	return nil
}

type decoder struct {
	raw    []byte
	pos    int
	values int
	limits Limits
}

func decode(raw []byte, limits Limits) (*value, error) {
	d := decoder{raw: raw, limits: limits}
	v, err := d.read(0)
	if err != nil {
		return nil, err
	}
	if d.pos != len(raw) {
		return nil, invalid("trailing_bytes", ErrInvalidMetainfo)
	}
	return v, nil
}

func (d *decoder) read(depth int) (*value, error) {
	if depth > d.limits.MaxDepth || d.values >= d.limits.MaxValues {
		return nil, invalid("bencode_limit", ErrLimitExceeded)
	}
	if d.pos >= len(d.raw) {
		return nil, invalid("bencode_truncated", ErrInvalidMetainfo)
	}
	d.values++
	start := d.pos
	v := &value{kind: d.raw[d.pos]}
	switch v.kind {
	case 'i':
		d.pos++
		end := bytes.IndexByte(d.raw[d.pos:], 'e')
		if end < 0 {
			return nil, invalid("integer_truncated", ErrInvalidMetainfo)
		}
		raw := d.raw[d.pos : d.pos+end]
		if len(raw) == 0 || len(raw) > 20 || len(raw) > 1 && raw[0] == '0' || raw[0] == '-' && (len(raw) == 1 || raw[1] == '0') {
			return nil, invalid("integer_invalid", ErrInvalidMetainfo)
		}
		for i, ch := range raw {
			if ch < '0' || ch > '9' {
				if i != 0 || ch != '-' {
					return nil, invalid("integer_invalid", ErrInvalidMetainfo)
				}
			}
		}
		n, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			return nil, invalid("integer_overflow", ErrInvalidMetainfo)
		}
		v.n = n
		d.pos += end + 1
	case 'l', 'd':
		d.pos++
		seen := map[string]bool{}
		for {
			if d.pos >= len(d.raw) {
				return nil, invalid("container_truncated", ErrInvalidMetainfo)
			}
			if d.raw[d.pos] == 'e' {
				d.pos++
				break
			}
			key := ""
			if v.kind == 'd' {
				b, err := d.readString()
				if err != nil {
					return nil, err
				}
				key = string(b)
				if seen[key] {
					return nil, invalid("duplicate_key", ErrInvalidMetainfo)
				}
				seen[key] = true
			}
			child, err := d.read(depth + 1)
			if err != nil {
				return nil, err
			}
			if v.kind == 'd' {
				v.dict = append(v.dict, pair{key, child})
			} else {
				v.list = append(v.list, child)
			}
		}
	default:
		b, err := d.readString()
		if err != nil {
			return nil, err
		}
		v.kind, v.data = 's', b
	}
	v.raw = d.raw[start:d.pos]
	return v, nil
}

func (d *decoder) readString() ([]byte, error) {
	start := d.pos
	for d.pos < len(d.raw) && d.raw[d.pos] >= '0' && d.raw[d.pos] <= '9' {
		d.pos++
	}
	if d.pos == start || d.pos-start > 9 || d.pos >= len(d.raw) || d.raw[d.pos] != ':' || d.pos-start > 1 && d.raw[start] == '0' {
		return nil, invalid("string_length_invalid", ErrInvalidMetainfo)
	}
	n, err := strconv.ParseUint(string(d.raw[start:d.pos]), 10, 32)
	if err != nil {
		return nil, invalid("string_length_invalid", ErrInvalidMetainfo)
	}
	d.pos++
	if n > uint64(len(d.raw)-d.pos) {
		return nil, invalid("string_truncated", ErrInvalidMetainfo)
	}
	out := d.raw[d.pos : d.pos+int(n)]
	d.pos += int(n)
	return out, nil
}
