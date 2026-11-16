//go:build !windows

package progress

import "os"

func enableVT(_ *os.File) func() {
	return func() {}
}
