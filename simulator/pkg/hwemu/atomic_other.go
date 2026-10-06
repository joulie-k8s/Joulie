//go:build !linux

package hwemu

import "os"

// nodeLease has no lease to take outside Linux: the caller writes unguarded.
func nodeLease(*os.File) (release func(), busy, ok bool) { return func() {}, false, false }
