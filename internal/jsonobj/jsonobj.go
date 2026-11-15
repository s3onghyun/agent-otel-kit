// Package jsonobj edits JSON objects while keeping the order of their keys,
// so a settings file changed by setup stays readable and diffs stay small.
package jsonobj

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Object is a JSON object whose keys keep their original order.
type Object struct {
	keys []string
	vals map[string]json.RawMessage
}

// New returns an empty object.
func New() *Object {
	return &Object{vals: map[string]json.RawMessage{}}
}

// Parse reads a JSON object. Empty or whitespace-only input is an empty object.
func Parse(data []byte) (*Object, error) {
	o := New()
	if len(bytes.TrimSpace(data)) == 0 {
		return o, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("top-level JSON value is not an object")
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("unexpected token %v", tok)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		o.Set(key, raw)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after JSON object")
	}
	return o, nil
}

// Keys returns the keys in order.
func (o *Object) Keys() []string {
	return append([]string(nil), o.keys...)
}

// Get returns the raw value of key.
func (o *Object) Get(key string) (json.RawMessage, bool) {
	v, ok := o.vals[key]
	return v, ok
}

// GetString returns the value of key if it is a JSON string.
func (o *Object) GetString(key string) (string, bool) {
	raw, ok := o.vals[key]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// GetObject returns the value of key parsed as an object. A missing key
// yields an empty object; a non-object value is an error.
func (o *Object) GetObject(key string) (*Object, error) {
	raw, ok := o.vals[key]
	if !ok {
		return New(), nil
	}
	sub, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", key, err)
	}
	return sub, nil
}

// Set stores a raw value, keeping the key's position if it already exists.
func (o *Object) Set(key string, raw json.RawMessage) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = raw
}

// SetValue marshals v and stores it.
func (o *Object) SetValue(key string, v any) error {
	raw, err := marshal(v)
	if err != nil {
		return err
	}
	o.Set(key, raw)
	return nil
}

// SetObject stores a nested object.
func (o *Object) SetObject(key string, sub *Object) {
	o.Set(key, sub.compact())
}

// Delete removes key.
func (o *Object) Delete(key string) {
	if _, ok := o.vals[key]; !ok {
		return
	}
	delete(o.vals, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// Len is the number of keys.
func (o *Object) Len() int { return len(o.keys) }

// Marshal renders the object with two-space indentation and a final newline.
func (o *Object) Marshal() ([]byte, error) {
	var out bytes.Buffer
	if err := json.Indent(&out, o.compact(), "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

func (o *Object) compact() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		var c bytes.Buffer
		if err := json.Compact(&c, o.vals[k]); err != nil {
			b.Write(o.vals[k])
		} else {
			b.Write(c.Bytes())
		}
	}
	b.WriteByte('}')
	return b.Bytes()
}

func marshal(v any) (json.RawMessage, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(strings.TrimRight(b.String(), "\n")), nil
}
