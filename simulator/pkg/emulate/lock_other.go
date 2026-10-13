//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package emulate

// lockRoot has no flock(2) to take on this platform: the root is not
// locked, and keeping a second simulator off it is up to the operator.
func lockRoot(string) (unlock func() error, err error) {
	return func() error { return nil }, nil
}
