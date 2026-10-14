//go:build !linux

package hwemu

import "time"

// nodeFileStamp records nothing outside Linux: every readback reads every
// A file.
type nodeFileStamp struct{}

func (s *nodeFileStamp) unchanged(string) bool { return false }

// nodeReadA reads the A file at path (nodeReadWithMTime) into *buf.
func nodeReadA(path string, buf *[]byte, _ *nodeFileStamp, _ time.Time) ([]byte, time.Time, error) {
	b, mtime, err := nodeReadWithMTime(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	*buf = append((*buf)[:0], b...)
	return *buf, mtime, nil
}
