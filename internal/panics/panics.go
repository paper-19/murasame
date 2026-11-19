package panics

import (
	"errors"
	"fmt"
)

func Catch[E error](fn func()) (caught E) {
	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok && errors.As(err, &caught) {
				return
			}
			panic(r)
		}
	}()
	fn()
	return caught
}

func AsError(r any) error {
	if err, ok := r.(error); ok {
		return err
	}
	return fmt.Errorf("%v", r)
}
