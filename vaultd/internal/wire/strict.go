// Package wire holds the vault v1 message formats (spec
// docs/specs/2026-10-vault-protocol.md, sections 4 to 7) and the strict JSON
// decoder every signed message goes through.
package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxInt is the largest integer a vault message may carry (2^53 - 1).
const MaxInt = 1<<53 - 1

var intRe = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// StrictDecode decodes data into v (a pointer to a struct) under the rules of
// spec section 4: one UTF-8 JSON object, no trailing data, no duplicate key at
// any depth, no unknown key, every key of the struct present, integers only
// in [0, 2^53-1]. Struct fields need a json tag; there are no optional keys.
func StrictDecode(data []byte, v any) error {
	if !utf8.Valid(data) {
		return errors.New("not utf-8")
	}
	if err := checkEscapes(data); err != nil {
		return err
	}
	if err := scan(data); err != nil {
		return err
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.Elem().Kind() != reflect.Struct {
		return errors.New("StrictDecode needs a pointer to a struct")
	}
	var raw any
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(&raw); err != nil {
		return err
	}
	if err := requireKeys(raw, rv.Elem().Type(), "$"); err != nil {
		return err
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	return nil
}

// scan walks the token stream once: a single top-level object, no duplicate
// keys, only canonical non-negative integers as numbers, nothing after it.
func scan(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	tok, err := d.Token()
	if err != nil {
		return err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return errors.New("top level is not an object")
	}
	if err := scanObject(d, 1); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

const maxDepth = 4

func scanObject(d *json.Decoder, depth int) error {
	if depth > maxDepth {
		return errors.New("nesting too deep")
	}
	seen := map[string]bool{}
	for {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		if delim, ok := tok.(json.Delim); ok && delim == '}' {
			return nil
		}
		key, ok := tok.(string)
		if !ok {
			return errors.New("object key is not a string")
		}
		if seen[key] {
			return fmt.Errorf("duplicate key %q", key)
		}
		seen[key] = true
		if err := scanValue(d, depth); err != nil {
			return err
		}
	}
}

func scanValue(d *json.Decoder, depth int) error {
	tok, err := d.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return scanObject(d, depth+1)
		case '[':
			if depth+1 > maxDepth {
				return errors.New("nesting too deep")
			}
			for d.More() {
				if err := scanValue(d, depth+1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil {
				return err
			}
			if end != json.Delim(']') {
				return errors.New("bad array")
			}
			return nil
		default:
			return errors.New("unexpected delimiter")
		}
	case json.Number:
		s := t.String()
		if !intRe.MatchString(s) || len(s) > 16 {
			return fmt.Errorf("number %q is not an integer in range", s)
		}
		var n uint64
		if _, err := fmt.Sscan(s, &n); err != nil || n > MaxInt {
			return fmt.Errorf("number %q out of range", s)
		}
		return nil
	case string, bool:
		return nil
	case nil:
		return errors.New("null is not allowed")
	}
	return errors.New("unexpected token")
}

// requireKeys checks that every json-tagged field of typ is present in raw,
// recursing into nested structs and slices of structs.
func requireKeys(raw any, typ reflect.Type, at string) error {
	switch typ.Kind() {
	case reflect.Struct:
		obj, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: want an object", at)
		}
		// Exact, case-sensitive key set: encoding/json would map "N" onto
		// field "n", so every key must be a schema key spelled exactly.
		names := map[string]bool{}
		for i := 0; i < typ.NumField(); i++ {
			if f := typ.Field(i); f.IsExported() {
				names[strings.Split(f.Tag.Get("json"), ",")[0]] = true
			}
		}
		for k := range obj {
			if !names[k] {
				return fmt.Errorf("%s: unknown key %q", at, k)
			}
		}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if !f.IsExported() {
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "" || name == "-" {
				return fmt.Errorf("field %s has no json tag", f.Name)
			}
			val, ok := obj[name]
			if !ok {
				return fmt.Errorf("%s: missing key %q", at, name)
			}
			if err := requireKeys(val, f.Type, at+"."+name); err != nil {
				return err
			}
		}
		return nil
	case reflect.Slice:
		if typ.Elem().Kind() == reflect.Uint8 {
			return nil
		}
		arr, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("%s: want an array", at)
		}
		for i, el := range arr {
			if err := requireKeys(el, typ.Elem(), fmt.Sprintf("%s[%d]", at, i)); err != nil {
				return err
			}
		}
		return nil
	case reflect.String:
		if _, ok := raw.(string); !ok {
			return fmt.Errorf("%s: want a string", at)
		}
	case reflect.Int, reflect.Int64, reflect.Uint64:
		if _, ok := raw.(json.Number); !ok {
			return fmt.Errorf("%s: want an integer", at)
		}
	case reflect.Bool:
		if _, ok := raw.(bool); !ok {
			return fmt.Errorf("%s: want a bool", at)
		}
	}
	return nil
}

// checkEscapes allows only \" \\ and \/ inside strings (spec section 4): no
// field may hold a control character, so no other escape is ever needed, and
// \u (unpaired surrogates) is ruled out.
func checkEscapes(data []byte) error {
	in := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if !in {
			if c == '"' {
				in = true
			}
			continue
		}
		switch c {
		case '"':
			in = false
		case '\\':
			if i+1 >= len(data) {
				return errors.New("bad escape")
			}
			switch data[i+1] {
			case '"', '\\', '/':
				i++
			default:
				return fmt.Errorf("escape \\%c not allowed", data[i+1])
			}
		default:
			if c < 0x20 {
				return errors.New("control byte in string")
			}
		}
	}
	return nil
}
