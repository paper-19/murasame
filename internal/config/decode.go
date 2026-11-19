package config

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/go-viper/mapstructure/v2"
)

const MaxInt = math.MaxInt32

type keyError struct {
	key string
	msg string
}

func (e *keyError) Error() string { return e.key + ": " + e.msg }

func Decode(v any, out any, what string, required ...string) error {
	m, err := tableOf(v)
	if err != nil {
		return Errf("%s: %s", what, err)
	}
	var missing []string
	for _, k := range required {
		if _, ok := m[k]; !ok {
			missing = append(missing, k)
		}
	}
	var meta mapstructure.Metadata
	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:     out,
		Metadata:   &meta,
		DecodeHook: typed,
		MatchName:  func(key, field string) bool { return key == field },
	})
	if err != nil {
		return err
	}
	derr := dec.Decode(m)
	if len(meta.Unused) > 0 {
		sort.Strings(meta.Unused)
		return Errf("%s: unknown key(s): %s", what, strings.Join(meta.Unused, ", "))
	}
	if len(missing) > 0 {
		return Errf("%s: missing required key(s): %s", what, strings.Join(missing, ", "))
	}
	if derr != nil {
		return located(what, derr)
	}
	return nil
}

func joinKey(what, key string) string {
	if what == "" || key == "" {
		return what + key
	}
	return what + "." + key
}

func located(what string, err error) error {
	var de *mapstructure.DecodeError
	for errors.As(err, &de) {
		what = joinKey(what, de.Name())
		err = de.Unwrap()
	}
	var ke *keyError
	if errors.As(err, &ke) {
		what = joinKey(what, ke.key)
		err = errors.New(ke.msg)
	}
	return Errf("%s: %s", what, err)
}

var (
	cmdType     = reflect.TypeOf(Cmd{})
	stringsType = reflect.TypeOf([]string(nil))
	strMapType  = reflect.TypeOf(map[string]string(nil))
)

func typed(from, to reflect.Value) (any, error) {
	v := from.Interface()
	switch to.Type() {
	case cmdType:
		args, err := argv(v)
		if err == nil && len(args) == 0 {
			err = errors.New("an empty command (it needs at least the program)")
		}
		return Cmd{Args: args}, err
	case stringsType:
		return Strings(v)
	case strMapType:
		return StringMap(v)
	}
	switch to.Kind() {
	case reflect.Int:
		return Int(v)
	case reflect.String:
		return String(v)
	case reflect.Bool:
		if b, ok := v.(bool); ok {
			return b, nil
		}
		return nil, fmt.Errorf("true or false, not %s", describe(v))
	case reflect.Struct, reflect.Map:
		return tableOf(v)
	case reflect.Slice:
		if _, ok := v.([]any); !ok {
			return nil, fmt.Errorf("an array of tables, not %s", kindOf(v))
		}
	}
	return v, nil
}

func Int(v any) (int, error) {
	n, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("a whole number, not %s", describe(v))
	}
	if n < 0 || n > MaxInt {
		return 0, fmt.Errorf("from 0 to %d, not %d", MaxInt, n)
	}
	return int(n), nil
}

func String(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	return "", fmt.Errorf("a string, not %s", describe(v))
}

func Strings(v any) ([]string, error) {
	x, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("a list of strings, not %s", describe(v))
	}
	out := make([]string, 0, len(x))
	for _, e := range x {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("a list of strings, not one with %s", describe(e))
		}
		out = append(out, s)
	}
	return out, nil
}

func StringMap(v any) (map[string]string, error) {
	m, err := tableOf(v)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(m))
	for k, x := range m {
		s, err := String(x)
		if err != nil {
			return nil, &keyError{k, err.Error()}
		}
		out[k] = s
	}
	return out, nil
}

func argv(v any) ([]string, error) {
	if s, ok := v.(string); ok {
		return nil, fmt.Errorf("a list of strings like [\"make\", \"check\"], not the string %q (for a shell line write "+
			"[\"sh\", \"-c\", %q])", s, s)
	}
	return Strings(v)
}

func Table(v any, what string) (map[string]any, error) {
	m, err := tableOf(v)
	if err != nil {
		return nil, Errf("%s: %s", what, err)
	}
	return m, nil
}

func tableOf(v any) (map[string]any, error) {
	if v == nil {
		return map[string]any{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("a table, not %s", kindOf(v))
	}
	return m, nil
}

func describe(v any) string {
	switch x := v.(type) {
	case string:
		return "the string " + strconv.Quote(x)
	case bool, int64, float64:
		return fmt.Sprintf("%s (%v)", kindOf(v), x)
	}
	return kindOf(v)
}

func kindOf(v any) string {
	switch v.(type) {
	case map[string]any:
		return "a table"
	case []any:
		return "an array"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case int64, int:
		return "a whole number"
	case float64:
		return "a number with a fraction"
	}
	return "a date or time"
}
