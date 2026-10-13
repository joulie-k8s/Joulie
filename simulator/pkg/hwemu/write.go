package hwemu

// writeS writes content to f when it differs from what f last held. Where
// the Node has leases, a value at least as long as the file is written in
// place (nodeWriteLeased); any other value, or one the in-place write
// refuses, replaces the file by temp file and rename (nodeWriteAtomic).
// Either way a reader sees the old value or the new one, whole.
//
// The in-place write is the common case: it creates and frees no inode and
// changes no directory, which on a disk-backed root were nearly all the cost
// of a step (BenchmarkEM01Tick in simulator/pkg/emulate).
func (n *Node) writeS(f *nodeSFile, content string) error {
	if f == nil || content == f.last {
		return nil
	}
	if n.leases && len(content) >= len(f.last) && nodeWriteLeased(f.path, f.last, content) {
		f.last = content
		return nil
	}
	if err := nodeWriteAtomic(n.tmpDir, f.path, []byte(content), f.mode); err != nil {
		return err
	}
	f.last = content
	return nil
}
