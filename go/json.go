// A tiny JSON engine: one value type, a parser for request bodies and a
// marshaller for responses. The forge never imports encoding/json: the
// standard library's decoder is general-purpose machinery this server does
// not need, and leaving it out keeps the static binary comfortably under
// the entry's 8 MiB per-file cap.
//
// Values are plain Go values: nil, bool, float64 (numbers), string,
// []any, map[string]any. Object key order on output follows insertion
// order for the ordered object type below; plain maps marshal with their
// keys sorted, which is deterministic and fine for the surfaces that use
// them (GraphQL and MCP build ordered objects explicitly).

package main

import (
	"strconv"
	"strings"
)

// JObj is an object whose keys marshal in insertion order.
type JObj struct {
	Keys []string
	Vals map[string]any
}

func NewJObj() *JObj { return &JObj{Vals: map[string]any{}} }

func (o *JObj) Set(key string, val any) *JObj {
	if _, ok := o.Vals[key]; !ok {
		o.Keys = append(o.Keys, key)
	}
	o.Vals[key] = val
	return o
}

func (o *JObj) Get(key string) (any, bool) {
	v, ok := o.Vals[key]
	return v, ok
}

// ---------------------------------------------------------------------------
// Marshalling
// ---------------------------------------------------------------------------

// Escape a string as a JSON string literal, fast when nothing needs
// escaping (the common case for names, titles and ids).
func jsonQuote(s string) string {
	need := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == '"' || c == '\\' {
			need = true
			break
		}
	}
	if !need {
		return `"` + s + `"`
	}
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte("0123456789abcdef"[c>>4])
				b.WriteByte("0123456789abcdef"[c&0xf])
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func marshalInto(b *strings.Builder, v any) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		b.WriteString(jsonQuote(t))
	case int:
		b.WriteString(strconv.Itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case float64:
		b.WriteString(strconv.FormatFloat(t, 'g', -1, 64))
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			marshalInto(b, e)
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sortStrings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(jsonQuote(k))
			b.WriteByte(':')
			marshalInto(b, t[k])
		}
		b.WriteByte('}')
	case *JObj:
		b.WriteByte('{')
		for i, k := range t.Keys {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(jsonQuote(k))
			b.WriteByte(':')
			marshalInto(b, t.Vals[k])
		}
		b.WriteByte('}')
	default:
		b.WriteString("null")
	}
}

// Marshal renders a value as compact JSON.
func Marshal(v any) string {
	var b strings.Builder
	marshalInto(&b, v)
	return b.String()
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

type jsonParser struct {
	data []byte
	pos  int
}

func (p *jsonParser) skipWS() {
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			p.pos++
		} else {
			break
		}
	}
}

func (p *jsonParser) parseValue() (any, bool) {
	p.skipWS()
	if p.pos >= len(p.data) {
		return nil, false
	}
	switch c := p.data[p.pos]; {
	case c == '{':
		return p.parseObject()
	case c == '[':
		return p.parseArray()
	case c == '"':
		s, ok := p.parseString()
		return s, ok
	case c == 't':
		if p.pos+4 <= len(p.data) && string(p.data[p.pos:p.pos+4]) == "true" {
			p.pos += 4
			return true, true
		}
		return nil, false
	case c == 'f':
		if p.pos+5 <= len(p.data) && string(p.data[p.pos:p.pos+5]) == "false" {
			p.pos += 5
			return false, true
		}
		return nil, false
	case c == 'n':
		if p.pos+4 <= len(p.data) && string(p.data[p.pos:p.pos+4]) == "null" {
			p.pos += 4
			return nil, true
		}
		return nil, false
	default:
		return p.parseNumber()
	}
}

func (p *jsonParser) parseObject() (any, bool) {
	p.pos++ // {
	obj := map[string]any{}
	p.skipWS()
	if p.pos < len(p.data) && p.data[p.pos] == '}' {
		p.pos++
		return obj, true
	}
	for {
		p.skipWS()
		key, ok := p.parseString()
		if !ok {
			return nil, false
		}
		p.skipWS()
		if p.pos >= len(p.data) || p.data[p.pos] != ':' {
			return nil, false
		}
		p.pos++
		val, ok := p.parseValue()
		if !ok {
			return nil, false
		}
		obj[key] = val
		p.skipWS()
		if p.pos >= len(p.data) {
			return nil, false
		}
		if p.data[p.pos] == '}' {
			p.pos++
			return obj, true
		}
		if p.data[p.pos] != ',' {
			return nil, false
		}
		p.pos++
	}
}

func (p *jsonParser) parseArray() (any, bool) {
	p.pos++ // [
	arr := []any{}
	p.skipWS()
	if p.pos < len(p.data) && p.data[p.pos] == ']' {
		p.pos++
		return arr, true
	}
	for {
		val, ok := p.parseValue()
		if !ok {
			return nil, false
		}
		arr = append(arr, val)
		p.skipWS()
		if p.pos >= len(p.data) {
			return nil, false
		}
		if p.data[p.pos] == ']' {
			p.pos++
			return arr, true
		}
		if p.data[p.pos] != ',' {
			return nil, false
		}
		p.pos++
	}
}

func (p *jsonParser) parseString() (string, bool) {
	if p.pos >= len(p.data) || p.data[p.pos] != '"' {
		return "", false
	}
	p.pos++
	var b strings.Builder
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		if c == '"' {
			p.pos++
			return b.String(), true
		}
		if c == '\\' && p.pos+1 < len(p.data) {
			e := p.data[p.pos+1]
			switch e {
			case 'n':
				b.WriteByte('\n')
				p.pos += 2
			case 't':
				b.WriteByte('\t')
				p.pos += 2
			case 'r':
				b.WriteByte('\r')
				p.pos += 2
			case 'b':
				b.WriteByte('\b')
				p.pos += 2
			case 'f':
				b.WriteByte('\f')
				p.pos += 2
			case '/':
				b.WriteByte('/')
				p.pos += 2
			case '\\':
				b.WriteByte('\\')
				p.pos += 2
			case '"':
				b.WriteByte('"')
				p.pos += 2
			case 'u':
				if p.pos+6 <= len(p.data) {
					hex := string(p.data[p.pos+2 : p.pos+6])
					if n, err := strconv.ParseUint(hex, 16, 32); err == nil {
						// Surrogate pairs: a high surrogate is followed by \uXXXX.
						if n >= 0xD800 && n <= 0xDBFF && p.pos+12 <= len(p.data) &&
							p.data[p.pos+6] == '\\' && p.data[p.pos+7] == 'u' {
							hex2 := string(p.data[p.pos+8 : p.pos+12])
							if m, err2 := strconv.ParseUint(hex2, 16, 32); err2 == nil && m >= 0xDC00 && m <= 0xDFFF {
								r := 0x10000 + (rune(n-0xD800) << 10) + rune(m-0xDC00)
								b.WriteRune(r)
								p.pos += 12
								continue
							}
						}
						b.WriteRune(rune(n))
						p.pos += 6
						continue
					}
				}
				return "", false
			default:
				return "", false
			}
			continue
		}
		b.WriteByte(c)
		p.pos++
	}
	return "", false
}

func (p *jsonParser) parseNumber() (any, bool) {
	start := p.pos
	if p.pos < len(p.data) && p.data[p.pos] == '-' {
		p.pos++
	}
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		if (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
			p.pos++
		} else {
			break
		}
	}
	if p.pos == start {
		return nil, false
	}
	text := string(p.data[start:p.pos])
	if f, err := strconv.ParseFloat(text, 64); err == nil {
		return f, true
	}
	return nil, false
}

// ParseJSON parses a whole JSON document. It reports false on any syntax error.
func ParseJSON(data []byte) (any, bool) {
	p := &jsonParser{data: data}
	v, ok := p.parseValue()
	if !ok {
		return nil, false
	}
	p.skipWS()
	if p.pos != len(p.data) {
		return nil, false
	}
	return v, true
}

// Helpers for reading fields out of a parsed object body.

func objMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func strField(m map[string]any, name string) (string, bool) {
	v, ok := m[name]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func boolField(m map[string]any, name string) (bool, bool) {
	v, ok := m[name]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func numField(m map[string]any, name string) (float64, bool) {
	v, ok := m[name]
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	return f, ok
}
