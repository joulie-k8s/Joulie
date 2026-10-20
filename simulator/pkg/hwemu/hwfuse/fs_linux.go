//go:build linux

package hwfuse

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// cacheTimeout is how long the kernel keeps an entry, an attribute or a
// missing name: the layout of a mount never changes, and content is never
// cached (FOPEN_DIRECT_IO).
const cacheTimeout = time.Hour

// Server is a mounted file system: Mount returns it, Unmount ends it.
type Server struct {
	srv *fuse.Server
	dir string
}

// Dir is the mount point.
func (s *Server) Dir() string { return s.dir }

// Unmount unmounts the file system and waits until its requests are served.
func (s *Server) Unmount() error {
	if err := s.srv.Unmount(); err != nil {
		return fmt.Errorf("hwfuse: unmount %s: %w", s.dir, err)
	}
	s.srv.Wait()
	return nil
}

// Mount mounts nodes at dir, an existing empty directory, and serves them
// until Unmount or the end of the process: a directory per node, named
// after it. As root it mounts with mount(2), which needs CAP_SYS_ADMIN and
// /dev/fuse, and lets every user in (allow_other); as another user it calls
// fusermount3, and only that user gets in.
func Mount(dir string, nodes []Node, o Options) (*Server, error) {
	if o.Clock == nil {
		o.Clock = time.Now
	}
	r := &rootDir{byName: map[string]*view{}, mtime: time.Now()}
	for k, n := range nodes {
		if n.Name == "" || n.Name == "." || n.Name == ".." || path.Base(n.Name) != n.Name {
			return nil, fmt.Errorf("hwfuse: node name %q is not a directory name", n.Name)
		}
		if r.byName[n.Name] != nil {
			return nil, fmt.Errorf("hwfuse: node %s is listed twice", n.Name)
		}
		if n.Backend == nil {
			return nil, fmt.Errorf("hwfuse: node %s has no backend", n.Name)
		}
		v := newView(k, n, o.Clock, r.mtime)
		r.byName[n.Name] = v
		r.names = append(r.names, n.Name)
	}
	sort.Strings(r.names)
	root := os.Geteuid() == 0
	timeout := cacheTimeout
	opts := &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName:        "hwemu",
			Name:          "hwemu",
			Options:       []string{"default_permissions"},
			AllowOther:    root,
			DirectMount:   root,
			DisableXAttrs: true,
		},
		EntryTimeout:    &timeout,
		AttrTimeout:     &timeout,
		NegativeTimeout: &timeout,
		NullPermissions: true,
		RootStableAttr:  &fs.StableAttr{Ino: 1},
	}
	srv, err := fs.Mount(dir, r, opts)
	if err != nil {
		return nil, fmt.Errorf("hwfuse: mount %s: %w", dir, err)
	}
	return &Server{srv: srv, dir: dir}, nil
}

// view is one node of a mount: its entries by path relative to the node
// root, the children of each directory ("" is the node root), and the
// inode numbers of its entries, k<<24 plus the entry's index plus one.
type view struct {
	name     string
	backend  Backend
	clock    func() time.Time
	uid, gid uint32
	mtime    time.Time
	files    []layout.FileSpec
	byRel    map[string]int
	children map[string][]int
	inoBase  uint64
}

func newView(k int, n Node, clock func() time.Time, mtime time.Time) *view {
	v := &view{
		name: n.Name, backend: n.Backend, clock: clock, uid: n.UID, gid: n.GID, mtime: mtime,
		files:    n.Files,
		byRel:    make(map[string]int, len(n.Files)),
		children: map[string][]int{},
		inoBase:  uint64(k+1) << 24,
	}
	for i, f := range n.Files {
		v.byRel[f.Rel] = i
	}
	for i, f := range n.Files {
		dir := path.Dir(f.Rel)
		if dir == "." {
			dir = ""
		}
		v.children[dir] = append(v.children[dir], i)
	}
	for dir, kids := range v.children {
		sort.Slice(kids, func(a, b int) bool { return v.files[kids[a]].Rel < v.files[kids[b]].Rel })
		v.children[dir] = kids
	}
	return v
}

// stable is the identity of entry i, -1 for the node root.
func (v *view) stable(i int) fs.StableAttr {
	if i < 0 {
		return fs.StableAttr{Mode: syscall.S_IFDIR, Ino: v.inoBase}
	}
	return fs.StableAttr{Mode: kindMode(v.files[i].Kind), Ino: v.inoBase + uint64(i) + 1}
}

func kindMode(k layout.Kind) uint32 {
	switch k {
	case layout.KindDir:
		return syscall.S_IFDIR
	case layout.KindSymlink:
		return syscall.S_IFLNK
	}
	return syscall.S_IFREG
}

// attr fills out with entry i as sysfs shows it, -1 for the node root.
func (v *view) attr(i int, out *fuse.Attr) {
	st := v.stable(i)
	perm := uint32(0o755)
	if i >= 0 {
		perm = uint32(v.files[i].Mode.Perm())
	}
	out.Ino = st.Ino
	out.Mode = st.Mode | perm
	out.Size = 0
	out.Nlink = 1
	switch st.Mode {
	case syscall.S_IFREG:
		out.Size = hwemu.LiveAttrSize
	case syscall.S_IFDIR:
		out.Nlink = 2
	case syscall.S_IFLNK:
		out.Mode = st.Mode | 0o777
	}
	out.Uid, out.Gid = v.uid, v.gid
	out.SetTimes(&v.mtime, &v.mtime, &v.mtime)
	out.Blksize = hwemu.LiveAttrSize
}

// child makes the inode of entry i under parent.
func (v *view) child(ctx context.Context, parent *fs.Inode, i int) *fs.Inode {
	var ops fs.InodeEmbedder
	switch v.files[i].Kind {
	case layout.KindDir:
		ops = &dirNode{v: v, idx: i}
	case layout.KindSymlink:
		ops = &linkNode{v: v, idx: i}
	default:
		ops = &fileNode{v: v, idx: i}
	}
	return parent.NewInode(ctx, ops, v.stable(i))
}

// lookup is Lookup of name in directory dir of the view.
func (v *view) lookup(ctx context.Context, parent *fs.Inode, dir, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	i, ok := v.byRel[path.Join(dir, name)]
	if !ok || name == "." || name == ".." {
		return nil, syscall.ENOENT
	}
	v.attr(i, &out.Attr)
	return v.child(ctx, parent, i), 0
}

// readdir lists directory dir of the view.
func (v *view) readdir(dir string) fs.DirStream {
	kids := v.children[dir]
	entries := make([]fuse.DirEntry, 0, len(kids))
	for _, i := range kids {
		st := v.stable(i)
		entries = append(entries, fuse.DirEntry{Name: path.Base(v.files[i].Rel), Mode: st.Mode, Ino: st.Ino})
	}
	return fs.NewListDirStream(entries)
}

// rootDir is the mount's root, a directory per node.
type rootDir struct {
	fs.Inode
	byName map[string]*view
	names  []string
	mtime  time.Time
}

var (
	_ fs.NodeLookuper  = (*rootDir)(nil)
	_ fs.NodeReaddirer = (*rootDir)(nil)
	_ fs.NodeGetattrer = (*rootDir)(nil)
)

func (r *rootDir) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	v := r.byName[name]
	if v == nil {
		return nil, syscall.ENOENT
	}
	v.attr(-1, &out.Attr)
	return r.NewInode(ctx, &dirNode{v: v, idx: -1}, v.stable(-1)), 0
}

func (r *rootDir) Readdir(context.Context) (fs.DirStream, syscall.Errno) {
	entries := make([]fuse.DirEntry, 0, len(r.names))
	for _, name := range r.names {
		entries = append(entries, fuse.DirEntry{Name: name, Mode: syscall.S_IFDIR, Ino: r.byName[name].inoBase})
	}
	return fs.NewListDirStream(entries), 0
}

func (r *rootDir) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Ino = 1
	out.Mode = syscall.S_IFDIR | 0o755
	out.Nlink = 2
	out.SetTimes(&r.mtime, &r.mtime, &r.mtime)
	return 0
}

// dirNode is a directory of a view: idx is its entry, -1 for the node root.
type dirNode struct {
	fs.Inode
	v   *view
	idx int
}

var (
	_ fs.NodeLookuper  = (*dirNode)(nil)
	_ fs.NodeReaddirer = (*dirNode)(nil)
	_ fs.NodeGetattrer = (*dirNode)(nil)
	_ fs.NodeCreater   = (*dirNode)(nil)
	_ fs.NodeMkdirer   = (*dirNode)(nil)
	_ fs.NodeMknoder   = (*dirNode)(nil)
	_ fs.NodeSymlinker = (*dirNode)(nil)
	_ fs.NodeLinker    = (*dirNode)(nil)
	_ fs.NodeUnlinker  = (*dirNode)(nil)
	_ fs.NodeRmdirer   = (*dirNode)(nil)
	_ fs.NodeRenamer   = (*dirNode)(nil)
)

func (d *dirNode) rel() string {
	if d.idx < 0 {
		return ""
	}
	return d.v.files[d.idx].Rel
}

func (d *dirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return d.v.lookup(ctx, &d.Inode, d.rel(), name, out)
}

func (d *dirNode) Readdir(context.Context) (fs.DirStream, syscall.Errno) {
	return d.v.readdir(d.rel()), 0
}

func (d *dirNode) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	d.v.attr(d.idx, &out.Attr)
	return 0
}

// A sysfs directory has no create, mkdir, mknod, symlink, link, unlink,
// rmdir or rename: the VFS fails a create with EACCES and the rest with
// EPERM.

func (d *dirNode) Create(context.Context, string, uint32, uint32, *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	return nil, nil, 0, syscall.EACCES
}

func (d *dirNode) Mkdir(context.Context, string, uint32, *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, syscall.EPERM
}

func (d *dirNode) Mknod(context.Context, string, uint32, uint32, *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, syscall.EPERM
}

func (d *dirNode) Symlink(context.Context, string, string, *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, syscall.EPERM
}

func (d *dirNode) Link(context.Context, fs.InodeEmbedder, string, *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, syscall.EPERM
}

func (d *dirNode) Unlink(context.Context, string) syscall.Errno { return syscall.EPERM }

func (d *dirNode) Rmdir(context.Context, string) syscall.Errno { return syscall.EPERM }

func (d *dirNode) Rename(context.Context, string, fs.InodeEmbedder, string, uint32) syscall.Errno {
	return syscall.EPERM
}

// linkNode is a symlink of a view.
type linkNode struct {
	fs.Inode
	v   *view
	idx int
}

var (
	_ fs.NodeReadlinker = (*linkNode)(nil)
	_ fs.NodeGetattrer  = (*linkNode)(nil)
)

func (l *linkNode) Readlink(context.Context) ([]byte, syscall.Errno) {
	return []byte(l.v.files[l.idx].Target), 0
}

func (l *linkNode) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	l.v.attr(l.idx, &out.Attr)
	return 0
}

// fileNode is a regular file of a view, its content the Backend's.
type fileNode struct {
	fs.Inode
	v   *view
	idx int
}

var (
	_ fs.NodeOpener    = (*fileNode)(nil)
	_ fs.NodeReader    = (*fileNode)(nil)
	_ fs.NodeWriter    = (*fileNode)(nil)
	_ fs.NodeGetattrer = (*fileNode)(nil)
	_ fs.NodeSetattrer = (*fileNode)(nil)
	_ fs.NodeFlusher   = (*fileNode)(nil)
	_ fs.NodeFsyncer   = (*fileNode)(nil)
)

// handle is an open file: the content the last read from offset 0 got,
// which later reads continue from.
type handle struct {
	mu      sync.Mutex
	content []byte
	have    bool
}

func (f *fileNode) rel() string { return f.v.files[f.idx].Rel }

func (f *fileNode) Open(_ context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&syscall.O_ACCMODE != syscall.O_RDONLY && f.v.files[f.idx].Mode.Perm()&0o222 == 0 {
		return nil, 0, syscall.EACCES
	}
	return &handle{}, fuse.FOPEN_DIRECT_IO, 0
}

func (f *fileNode) Read(_ context.Context, fh fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h, ok := fh.(*handle)
	if !ok {
		h = &handle{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if off == 0 || !h.have {
		b, err := f.v.backend.LiveRead(f.rel(), f.v.clock())
		if err != nil {
			return nil, errnoOf(err)
		}
		h.content, h.have = b, true
	}
	if off >= int64(len(h.content)) {
		return fuse.ReadResultData(nil), 0
	}
	end := min(off+int64(len(dest)), int64(len(h.content)))
	return fuse.ReadResultData(h.content[off:end]), 0
}

func (f *fileNode) Write(_ context.Context, fh fs.FileHandle, data []byte, _ int64) (uint32, syscall.Errno) {
	k, err := f.v.backend.LiveWrite(f.rel(), data, f.v.clock())
	if err != nil {
		return 0, errnoOf(err)
	}
	if h, ok := fh.(*handle); ok {
		h.mu.Lock()
		h.have = false
		h.mu.Unlock()
	}
	return uint32(k), 0
}

func (f *fileNode) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	f.v.attr(f.idx, &out.Attr)
	return 0
}

// Setattr takes a truncation, which os.WriteFile and O_TRUNC ask for and
// sysfs ignores, and a change of times, and refuses a change of mode or
// owner.
func (f *fileNode) Setattr(_ context.Context, _ fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if in.Valid&(fuse.FATTR_MODE|fuse.FATTR_UID|fuse.FATTR_GID) != 0 {
		return syscall.EPERM
	}
	f.v.attr(f.idx, &out.Attr)
	return 0
}

func (f *fileNode) Flush(context.Context, fs.FileHandle) syscall.Errno { return 0 }

func (f *fileNode) Fsync(context.Context, fs.FileHandle, uint32) syscall.Errno { return 0 }

// errnoOf is the errno err wraps, EIO for an error that wraps none.
func errnoOf(err error) syscall.Errno {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}
	return syscall.EIO
}
