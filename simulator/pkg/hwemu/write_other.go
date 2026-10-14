//go:build !linux

package hwemu

// nodeWriteLeased never writes in place outside Linux, where a Node has no
// leases (nodeLease): every S write there goes by rename.
func nodeWriteLeased(path, last, content string) bool { return false }

// nodeNoFollow adds nothing outside Linux, where Nodes are only built for
// tests.
const nodeNoFollow = 0
