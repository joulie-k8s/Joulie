//go:build !linux

package hwfuse

// Server is a mounted file system; outside Linux there is none.
type Server struct{}

// Dir is the mount point.
func (s *Server) Dir() string { return "" }

// Unmount fails outside Linux.
func (s *Server) Unmount() error { return ErrUnsupported }

// Mount fails outside Linux.
func Mount(string, []Node, Options) (*Server, error) { return nil, ErrUnsupported }

// Bind fails outside Linux.
func Bind(string, string) error { return ErrUnsupported }
