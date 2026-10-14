package hwemu

// writeS writes content to f when it differs from what f last held. Where
// the Node has leases, a value at least as long as the file is written in
// place (nodeWriteLeased); any other value, or one the in-place write
// refuses, replaces the file by temp file and rename (WriteFileAtomic).
// Either way a reader sees the old value or the new one, whole.
//
// The in-place write is the common case: it creates and frees no inode and
// changes no directory, which on a disk-backed root were nearly all the cost
// of a step (BenchmarkEM01Tick in simulator/pkg/emulate).
func (n *Node) writeS(f *nodeSFile, content string) error {
	if f == nil {
		return nil
	}
	if n.unobserved {
		// Held until the next Step or Flush (StepUnobserved); f.last stays
		// what the file holds.
		if content == f.last {
			delete(n.pendingS, f)
		} else {
			if n.pendingS == nil {
				n.pendingS = map[*nodeSFile]string{}
			}
			n.pendingS[f] = content
		}
		return nil
	}
	delete(n.pendingS, f)
	if content == f.last {
		return nil
	}
	if n.leases && len(content) >= len(f.last) && nodeWriteLeased(f.path, f.last, content) {
		f.last = content
		return nil
	}
	if err := WriteFileAtomic(n.tmpDir, nodeAtomicPrefix, f.path, []byte(content), f.mode); err != nil {
		return err
	}
	f.last = content
	return nil
}
