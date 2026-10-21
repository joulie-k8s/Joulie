// Package hwfuse serves the trees of emulated nodes through FUSE, so their
// files behave as sysfs files do for a reader that only makes ordinary file
// calls, such as the agent: a value is computed when it is read and a write
// is answered when it is made.
//
// One mount serves any number of nodes, a directory per node holding the
// entries its Files list, as the tree's layout lays them out (with
// hwemu.Tree.LiveFiles, its sys/ and state/). The content of every regular
// file comes from the node's Backend, a Live hwemu.Node:
//
//   - A read from offset 0 asks the Backend for the whole content, and later
//     reads of the same open file continue from that content, as a sysfs
//     seq_file does, so a read in pieces is never torn. The kernel caches no
//     content (FOPEN_DIRECT_IO): every read reaches the Backend.
//   - Every write(2) is one call to the Backend, whatever its offset, which
//     returns the errno a refused value fails with. Truncation, as
//     os.WriteFile asks for it, changes nothing.
//   - Opening a file that has no write permission for writing fails with
//     EACCES whoever opens it, as on sysfs (kernfs_fop_open); any other
//     permission is checked by the kernel against the entry's mode and owner
//     (default_permissions), so root without CAP_DAC_OVERRIDE cannot write
//     a 0444 file either.
//   - Entries look as sysfs shows them: a regular file is
//     hwemu.LiveAttrSize bytes whatever its content, a directory and a
//     symlink 0, every link keeps its target, and nothing can be created,
//     removed or renamed (EACCES for a create, EPERM for the rest).
//
// Inode numbers are stable: a node's entry keeps its number for as long as
// the mount lives, whatever the kernel forgets.
package hwfuse

import (
	"errors"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// Backend is the content of one node's files: *hwemu.Node started with
// Options.Live. rel is the path of a regular file relative to the node
// root, and at the time of the call.
type Backend interface {
	LiveRead(rel string, at time.Time) ([]byte, error)
	LiveWrite(rel string, data []byte, at time.Time) (int, error)
}

// Node is one node a mount serves, in the directory Name: the entries of
// Files, every one of them owned by UID and GID, with Backend for the
// content.
type Node struct {
	Name     string
	Files    []layout.FileSpec
	Backend  Backend
	UID, GID uint32
}

// Options configure a mount. Clock is the time every call passes to a
// Backend, time.Now when nil: the clock the nodes step on.
type Options struct {
	Clock func() time.Time
}

// ErrUnsupported is the error of Mount and Bind outside Linux.
var ErrUnsupported = errors.New("hwfuse: FUSE views are served on Linux only")
