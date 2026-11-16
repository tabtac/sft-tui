//go:build !windows

package console

func Auto() bool              { return false }
func prepare() (func(), bool) { return func() {}, false }
