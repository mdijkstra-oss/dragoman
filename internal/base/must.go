package base

// Must turns a bootstrap-time failure into a panic. It is for values that are
// compiled in and therefore cannot be wrong at runtime: if one of those fails
// to parse, no caller could have handled it and the build is broken. Anything
// an operator supplies returns its error instead.
func Must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}
