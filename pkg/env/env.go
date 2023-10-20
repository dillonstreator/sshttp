package env

import (
	"encoding/base64"
	"fmt"
	"os"
	"reflect"
	"strconv"
)

type getOpts[T any] struct {
	parser func(v string) (T, error)
}

type getOptFn[T any] func(*getOpts[T])

// WithParser enables providing a custom parser for unsupported types fo Get
func WithParser[T any](parser func(v string) (T, error)) getOptFn[T] {
	return func(eo *getOpts[T]) {
		eo.parser = parser
	}
}

// Get parses the provided key out of the environment into T's type falling back to def if not found
// supported types are bool, []byte, int, and string
// []byte type will have the environment string base64 decoded
// a custom parser should be used for any type outside the supported set using WithParser
func Get[T any](key string, def T, optFns ...getOptFn[T]) T {
	opts := &getOpts[T]{}
	for _, optFn := range optFns {
		optFn(opts)
	}

	v, exists := os.LookupEnv(key)
	if !exists {
		return def
	}

	if opts.parser != nil {
		v, err := opts.parser(v)
		if err != nil {
			return def
		}

		return v
	}

	var out T
	switch ptr := any(&out).(type) {
	case *bool:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return def
		}

		*ptr = b

	case *[]byte:
		dec, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return def
		}

		*ptr = dec

	case *int:
		i, err := strconv.Atoi(v)
		if err != nil {
			return def
		}

		*ptr = i

	case *int64:
		i, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return def
		}

		*ptr = i

	case *string:
		*ptr = v

	default:
		panic(fmt.Sprintf("unsupported env type %s .. use a custom parser", reflect.TypeOf(out)))
	}

	return out
}
