package catfs

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"

	e "github.com/pkg/errors"
	c "github.com/sahib/brig/catfs/core"
	"github.com/sahib/brig/catfs/db"
	ie "github.com/sahib/brig/catfs/errors"
	"github.com/sahib/brig/catfs/mio"
	"github.com/sahib/brig/catfs/mio/compress"
	n "github.com/sahib/brig/catfs/nodes"
	"github.com/sahib/brig/catfs/vcs"
	"github.com/sahib/brig/util"
	h "github.com/sahib/brig/util/hashlib"
)

// FS (short for Filesystem) is the central API entry for everything related to
// paths.  It exposes a POSIX-like interface where path are mapped to the
// actual underlying hashes and the associated metadata.
//
// Additionally it supports version control commands like MakeCommit(),
// Checkout() etc.  The API is file-centric, i.e. directories are created on
// the fly for some operations like Stage(). Empty directories can be created
// via Mkdir() though.
type FS struct {
	mu sync.Mutex

	// underlying key/value store
	kv db.Database

	// linker (holds all nodes together)
	lkr *c.Linker

	// garbage collector for dead metadata links
	gc *c.GarbageCollector

	// ticker that drives the gc background routine
	gcTicker *time.Ticker

	// channel to schedule gc runs and quit the gc
	gcControl chan bool

	// Actual storage backend (e.g. ipfs or memory)
	bk FsBackend

	// internal config
	cfg *config
}

// StatInfo describes the metadata of a single node.
// The concept is comparable to the POSIX stat() call.
type StatInfo struct {
	Path       string
	Hash       h.Hash
	User       string
	Size       uint64
	Inode      uint64
	IsDir      bool
	Depth      int
	ModTime    time.Time
	IsPinned   bool
	IsExplicit bool
	Content    h.Hash
}

type DiffPair struct {
	Src StatInfo
	Dst StatInfo
}

type Diff struct {
	Added   []StatInfo
	Removed []StatInfo
	Ignored []StatInfo
	Missing []StatInfo

	Moved    []DiffPair
	Merged   []DiffPair
	Conflict []DiffPair
}

// Commit gives information about a single commit.
// TODO: Decide on naming: rev(ision), refname or tag.
type Commit struct {
	Hash h.Hash
	Msg  string
	Tags []string
	Date time.Time
}

type Change struct {
	Path    string
	Change  string
	ReferTo string
	Head    *Commit
	Next    *Commit
}

/////////////////////
// UTILITY HELPERS //
/////////////////////

func (fs *FS) nodeToStat(nd n.Node) *StatInfo {
	isPinned, isExplicit, err := fs.isPinned(nd)
	if err != nil {
		log.Warningf("stat: failed to acquire pin state: %v", err)
	}

	var content h.Hash
	if file, ok := nd.(*n.File); ok {
		content = file.Content()
	}

	return &StatInfo{
		Path:       nd.Path(),
		Hash:       nd.Hash().Clone(),
		User:       nd.User(),
		ModTime:    nd.ModTime(),
		IsDir:      nd.Type() == n.NodeTypeDirectory,
		Inode:      nd.Inode(),
		Size:       nd.Size(),
		Depth:      n.Depth(nd),
		IsPinned:   isPinned,
		IsExplicit: isExplicit,
		Content:    content,
	}
}

func lookupFileOrDir(lkr *c.Linker, path string) (n.ModNode, error) {
	nd, err := lkr.LookupNode(path)
	if err != nil {
		return nil, err
	}

	if nd == nil || nd.Type() == n.NodeTypeGhost {
		return nil, ie.NoSuchFile(path)
	}

	modNd, ok := nd.(n.ModNode)
	if !ok {
		return nil, ie.ErrBadNode
	}

	return modNd, nil
}

func (fs *FS) handleGcEvent(nd n.Node) bool {
	if nd.Type() != n.NodeTypeFile {
		return true
	}

	file, ok := nd.(*n.File)
	if !ok {
		return true
	}

	content := file.Content()
	log.Infof("unpinning gc'd node %v", content.B58String())

	// This node will not be reachable anymore by brig.
	// Make sure it is also unpinned to save space.
	if err := fs.bk.Unpin(file.Content(), true); err != nil {
		log.Warningf("unpinning attempt failed: %v", err)
	}

	// Still return true, no need to stop the GC
	return true
}

///////////////////////////////
// ACTUAL API IMPLEMENTATION //
///////////////////////////////

func (fs *FS) doGcRun() {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	owner, err := fs.lkr.Owner()
	if err != nil {
		log.Warningf("gc: failed to get owner: %v", err)
		return
	}

	log.Debugf("filesystem GC (for %s): running", owner)
	if err := fs.gc.Run(true); err != nil {
		log.Warnf("failed to run GC: %v", err)
	}
}

func NewFilesystem(backend FsBackend, dbPath string, owner string, cfg *Config) (*FS, error) {
	vfg, err := cfg.parseConfig()
	if err != nil {
		return nil, fmt.Errorf("Failed to parse config: %v", err)
	}

	kv, err := db.NewDiskDatabase(dbPath)
	if err != nil {
		return nil, err
	}

	lkr := c.NewLinker(kv)

	if err := lkr.SetOwner(owner); err != nil {
		return nil, err
	}

	fs := &FS{
		kv:        kv,
		lkr:       lkr,
		bk:        backend,
		cfg:       vfg,
		gcControl: make(chan bool),
		gcTicker:  time.NewTicker(120 * time.Second),
	}

	fs.gc = c.NewGarbageCollector(lkr, kv, fs.handleGcEvent)

	go func() {
		select {
		case state := <-fs.gcControl:
			if state {
				fs.doGcRun()
			} else {
				// Quit the gc loop:
				log.Debugf("Quitting the GC loop")
				return
			}
		case <-fs.gcTicker.C:
			fs.doGcRun()
		}
	}()

	return fs, nil
}

func (fs *FS) Close() error {
	go func() {
		fs.gcControl <- false
	}()

	fs.gcTicker.Stop()
	return fs.kv.Close()
}

func (fs *FS) Export(w io.Writer) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	return fs.kv.Export(w)
}

func (fs *FS) Import(r io.Reader) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if err := fs.kv.Import(r); err != nil {
		return err
	}

	// disk (probably) changed, delete memcache:
	fs.lkr.MemIndexClear()
	return nil
}

/////////////////////
// CORE OPERATIONS //
/////////////////////

func (fs *FS) Move(src, dst string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	srcNd, err := lookupFileOrDir(fs.lkr, src)
	if err != nil {
		return err
	}

	return c.Move(fs.lkr, srcNd, dst)
}

func (fs *FS) Copy(src, dst string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	srcNd, err := lookupFileOrDir(fs.lkr, src)
	if err != nil {
		return err
	}

	_, err = c.Copy(fs.lkr, srcNd, dst)
	return err
}

func (fs *FS) Mkdir(path string, createParents bool) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	_, err := c.Mkdir(fs.lkr, path, createParents)
	return err
}

func (fs *FS) Remove(path string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	nd, err := lookupFileOrDir(fs.lkr, path)
	if err != nil {
		return err
	}

	_, _, err = c.Remove(fs.lkr, nd, true, true)
	return err
}

func (fs *FS) Stat(path string) (*StatInfo, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	nd, err := fs.lkr.LookupNode(path)
	if err != nil {
		return nil, err
	}

	if nd.Type() == n.NodeTypeGhost {
		return nil, ie.NoSuchFile(path)
	}

	return fs.nodeToStat(nd), nil
}

// List returns stat info for each node below (and including) root.
// Nodes deeper than maxDepth will not be shown. If maxDepth is a
// negative number, all nodes will be shown.
func (fs *FS) List(root string, maxDepth int) ([]*StatInfo, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// NOTE: This method is highly inefficient:
	//       - iterates over all nodes even if maxDepth is >= 0
	//
	// Fix whenever it proves to be a problem.
	// I don't want to engineer something now until I know what's needed.
	rootNd, err := fs.lkr.LookupNode(root)
	if err != nil {
		return nil, err
	}

	if rootNd.Type() == n.NodeTypeGhost {
		return nil, ie.NoSuchFile(root)
	}

	// Start counting max depth relative to the root:
	if maxDepth >= 0 {
		maxDepth += n.Depth(rootNd)
	}

	result := []*StatInfo{}
	err = n.Walk(fs.lkr, rootNd, false, func(child n.Node) error {
		if maxDepth < 0 || n.Depth(child) <= maxDepth {
			if maxDepth >= 0 && child.Path() == root {
				return nil
			}

			// Ghost nodes should not be visible to the outside.
			if child.Type() == n.NodeTypeGhost {
				return nil
			}

			result = append(result, fs.nodeToStat(child))
		}

		return nil
	})

	sort.Slice(result, func(i, j int) bool {
		iDepth := result[i].Depth
		jDepth := result[j].Depth

		if iDepth == jDepth {
			return result[i].Path < result[j].Path
		}

		return iDepth < jDepth
	})

	if err != nil {
		return nil, err
	}

	return result, nil
}

////////////////////////
// PINNING OPERATIONS //
////////////////////////

func (fs *FS) pinOp(path string, explicit bool, op func(h.Hash, bool) error) error {
	nd, err := fs.lkr.LookupNode(path)
	if err != nil {
		return err
	}

	return n.Walk(fs.lkr, nd, true, func(child n.Node) error {
		if child.Type() == n.NodeTypeFile {
			file, ok := child.(*n.File)
			if !ok {
				return ie.ErrBadNode
			}

			if err := op(file.Content(), explicit); err != nil {
				return err
			}
		}

		return nil
	})
}

// preCache makes the backend fetch the data already from the network,
// even though it might not be needed yet.
func (fs *FS) preCache(path string) error {
	stream, err := fs.Cat(path)
	if err != nil {
		return err
	}

	_, err = io.Copy(ioutil.Discard, stream)
	return err
}

func (fs *FS) preCacheInBackground(path string) {
	go func() {
		if err := fs.preCache(path); err != nil {
			log.Debugf("failed to pre-cache `%s`: %v", path, err)
		}
	}()
}

// TODO: PIN: Make pre fetch configurable.
func (fs *FS) Pin(path string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if err := fs.pinOp(path, true, fs.bk.Pin); err != nil {
		return err
	}

	// Make sure the data is available:
	// (this is some sort of `cat path > /dev/null`)
	fs.preCacheInBackground(path)
	return nil
}

func (fs *FS) Unpin(path string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	return fs.pinOp(path, true, fs.bk.Unpin)
}

type ExplicitPin struct {
	Path   string
	Commit string
}

func (fs *FS) ListExplicitPins(prefix, fromRef, toRef string) ([]ExplicitPin, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	var from, to *n.Commit
	var err error

	if fromRef != "" {
		from, err = parseRev(fs.lkr, fromRef)
		if err != nil {
			return nil, e.Wrapf(err, "parse from ref")
		}
	}

	if toRef != "" {
		to, err = parseRev(fs.lkr, toRef)
		if err != nil {
			return nil, e.Wrapf(err, "parse to ref")
		}
	}

	results := []ExplicitPin{}
	err = fs.lkr.IterAll(from, to, func(nd n.ModNode, cmt *n.Commit) error {
		if nd.Type() != n.NodeTypeFile {
			return nil
		}

		if !strings.HasPrefix(nd.Path(), prefix) {
			return nil
		}

		_, isExplicit, err := fs.bk.IsPinned(nd.Content())
		if err != nil {
			return err
		}

		// isExplicit implies isPinned.
		if isExplicit {
			results = append(results, ExplicitPin{
				Path:   nd.Path(),
				Commit: cmt.Hash().B58String(),
			})
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return results, nil
}

func (fs *FS) ClearExplicitPins(prefix, fromRef, toRef string) (int, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	return fs.setExplicitPins(false, prefix, fromRef, toRef)
}

func (fs *FS) SetExplicitPin(prefix, fromRef, toRef string) (int, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	return fs.setExplicitPins(true, prefix, fromRef, toRef)
}

func (fs *FS) setExplicitPins(doPin bool, prefix, fromRef, toRef string) (int, error) {
	var err error
	var from, to *n.Commit

	if fromRef != "" {
		from, err = parseRev(fs.lkr, fromRef)
		if err != nil {
			return 0, e.Wrapf(err, "parse from ref")
		}
	}

	if toRef != "" {
		to, err = parseRev(fs.lkr, toRef)
		if err != nil {
			return 0, e.Wrapf(err, "parse to ref")
		}
	}

	processed := 0

	return processed, fs.lkr.IterAll(from, to, func(nd n.ModNode, cmt *n.Commit) error {
		if nd.Type() != n.NodeTypeFile {
			return nil
		}

		if !strings.HasPrefix(nd.Path(), prefix) {
			return nil
		}

		pinOp := fs.bk.Unpin
		if doPin {
			pinOp = fs.bk.Pin
		}

		if err := pinOp(nd.Content(), true); err != nil {
			return err
		}

		processed++
		return nil
	})
}

// errNotPinnedSentinel is returned to signal an early exit in Walk()
var errNotPinnedSentinel = errors.New("not pinned")

// IsPinned returns true for files and directories that are pinned.
// A directory only counts as pinned if all files and directories
// in it are also pinned.
func (fs *FS) IsPinned(path string) (bool, bool, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	nd, err := fs.lkr.LookupNode(path)
	if err != nil {
		return false, false, err
	}

	return fs.isPinned(nd)
}

// isPinned checks if `nd` is pinned.
// In case of directories, this returns true, if all files inside are pinned.
//
// TODO: This can be slow, since it relies on the backend.
//       Especially when backend has to check heavily recursive pins,
//       this gets slow. In this case we should maintain a cache of pins,
//       to avoid the costly re-calculation of deep directories.
//       This should alleviate the performance issues with big hierarchies,
//       and all code that uses e.g. Stat() (== pretty much everything)
func (fs *FS) isPinned(nd n.Node) (bool, bool, error) {
	pinCount := 0
	explicitCount := 0
	totalCount := 0

	err := n.Walk(fs.lkr, nd, true, func(child n.Node) error {
		if child.Type() == n.NodeTypeFile {
			file, ok := child.(*n.File)
			if !ok {
				return ie.ErrBadNode
			}

			totalCount++

			isPinned, isExplicit, err := fs.bk.IsPinned(file.Content())
			if err != nil {
				return err
			}

			if isExplicit {
				explicitCount++
			}

			if isPinned {
				// Make sure that we do not count empty directories
				// as pinned nodes.
				pinCount++
			} else {
				// Return a special error here to stop Walk() iterating.
				// One file is enough to stop IsPinned() from being true.
				return errNotPinnedSentinel
			}
		}

		return nil
	})

	if err != nil && err != errNotPinnedSentinel {
		return false, false, err
	}

	if err == errNotPinnedSentinel {
		return false, false, nil
	}

	return pinCount > 0, explicitCount == totalCount, nil
}

////////////////////////
// STAGING OPERATIONS //
////////////////////////

func prefixSlash(s string) string {
	if !strings.HasPrefix(s, "/") {
		return "/" + s
	}

	return s
}

// Touch creates an empty file at `path` if it does not exist yet.
// If it exists, it's mod time is being updated to the current time.
func (fs *FS) Touch(path string) error {
	nd, err := fs.lkr.LookupNode(path)
	if err != nil && !ie.IsNoSuchFileError(err) {
		return err
	}

	if nd != nil {
		modNd, ok := nd.(n.ModNode)
		if !ok {
			// Probably a ghost node.
			return nil
		}

		modNd.SetModTime(time.Now())
		return nil
	}

	// Notthing there, stage an empty file.
	return fs.Stage(prefixSlash(path), bytes.NewReader([]byte{}))
}

// Truncate cuts of the output of the file at `path` to `size`.
// `size` should be between 0 and the size of the file,
// all other values will be ignored.
//
// Note that this is not implemented as an actual IO operation.
// It is possible to go back to a bigger size until the actual
// content was changed via Stage().
func (fs *FS) Truncate(path string, size uint64) error {
	nd, err := fs.lkr.LookupModNode(path)
	if err != nil {
		return err
	}

	if nd.Type() != n.NodeTypeFile {
		return fmt.Errorf("`%s` is not a file", path)
	}

	nd.SetSize(size)
	return fs.lkr.StageNode(nd)
}

func peekHeader(r io.Reader) ([]byte, io.Reader, error) {
	headerBuf := make([]byte, 4*1024)
	n, err := r.Read(headerBuf)
	if err != nil && err != io.EOF {
		return nil, nil, err
	}

	headerBuf = headerBuf[:n]
	return headerBuf, util.PrefixReader(headerBuf, r), nil
}

// Stage reads all data from `r` and stores as content of the node at `path`.
// If `path` already exists, it will be updated.
func (fs *FS) Stage(path string, r io.Reader) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	path = prefixSlash(path)

	// See if we already have such a file.
	// If not we gonna need to generate new key for it
	// based on the content hash.
	var oldFile *n.File

	oldNode, err := fs.lkr.LookupNode(path)

	// Check that we're handling the right kind of node.
	// We should be able to add on-top of ghosts, but directorie
	// are pointless as input.
	if err == nil {
		switch oldNode.Type() {
		case n.NodeTypeDirectory:
			return fmt.Errorf("Cannot stage over directory: %v", path)
		case n.NodeTypeGhost:
			// Act like there was no such node:
			err = ie.NoSuchFile(path)
		case n.NodeTypeFile:
			var ok bool
			oldFile, ok = oldNode.(*n.File)
			if !ok {
				return ie.ErrBadNode
			}
		}
	}

	if err != nil && !ie.IsNoSuchFileError(err) {
		return err
	}

	// Get the size directly from the number of bytes written
	// to the backend and do not rely on external sources.
	sizeAcc := &util.SizeAccumulator{}
	sizeR := io.TeeReader(r, sizeAcc)

	var key []byte
	if oldFile == nil {
		// Generate a new key, we do not know this file yet.
		key = make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return err
		}
	} else {
		key = oldFile.Key()
	}

	// Read a small portion of the file header and use it
	// to determine what compression algorithm we can use.
	headerBuf, prefixR, err := peekHeader(sizeR)
	if err != nil {
		return err
	}

	algo, err := compress.GuessAlgorithm(path, headerBuf)
	if err != nil {
		algo = fs.cfg.compressAlgo
		log.Warningf("failed to guess suitable zip algo: %v", err)
	}

	log.Debugf("using '%s' compression for file %s", algo, path)

	stream, err := mio.NewInStream(prefixR, key, algo)
	if err != nil {
		return err
	}

	contentHash, err := fs.bk.Add(stream)
	if err != nil {
		return err
	}

	pinExplicit := false

	if oldFile != nil {
		if oldFile.Content().Equal(contentHash) {
			// Nothing changed.
			return nil
		}

		_, isExplicit, err := fs.bk.IsPinned(oldFile.Content())
		if err != nil {
			return err
		}

		// If the old file was pinned explicitly, we should also pin
		// the new file explicitly to carry over that info.
		pinExplicit = isExplicit

		// Unpin old content by force.
		if err := fs.bk.Unpin(oldFile.Content(), true); err != nil {
			return err
		}
	}

	if err := fs.bk.Pin(contentHash, pinExplicit); err != nil {
		return err
	}

	_, err = c.Stage(fs.lkr, path, contentHash, sizeAcc.Size(), key)
	return err
}

////////////////////
// I/O OPERATIONS //
////////////////////

// Cat will open a file read-only and expose it's underlying data as stream.
// If no such path is known or it was deleted, nil is returned as stream.
func (fs *FS) Cat(path string) (mio.Stream, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	file, err := fs.lkr.LookupFile(path)
	if err == ie.ErrBadNode {
		return nil, ie.NoSuchFile(path)
	}

	if err != nil {
		return nil, err
	}

	rawStream, err := fs.bk.Cat(file.Content())
	if err != nil {
		return nil, err
	}

	// TODO: This can still seek over boundaries?
	//       Exchange with proper limitReadSeeker + WriterTo?
	stream, err := mio.NewOutStream(rawStream, file.Key())
	if err != nil {
		return nil, err
	}

	// Truncate stream to file size. Data stream might be bigger
	// for example when fuse decided to truncate the file, but
	// did not flush it already.
	return mio.LimitStream(stream, file.Size()), nil
}

// Open returns a file like object that can be used for modifying a file in memory.
// If you want to have seekable read-only stream, use Cat(), it has less overhead.
func (fs *FS) Open(path string) (*Handle, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	nd, err := fs.lkr.LookupNode(path)
	if err != nil {
		return nil, err
	}

	file, ok := nd.(*n.File)
	if !ok {
		return nil, fmt.Errorf("Can only open files: %v", path)
	}

	return newHandle(fs, file), nil
}

////////////////////
// VCS OPERATIONS //
////////////////////

// MakeCommit bundles all staged changes into one commit described by `msg`.
// If no changes were made since the last call to MakeCommit() ErrNoConflict
// is returned.
func (fs *FS) MakeCommit(msg string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	owner, err := fs.lkr.Owner()
	if err != nil {
		return err
	}

	return fs.lkr.MakeCommit(owner, msg)
}

func (fs *FS) Head() (string, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	head, err := fs.lkr.Head()
	if err != nil {
		return "", err
	}

	return head.Hash().B58String(), nil
}

// History returns all modifications of a node with one entry per commit.
func (fs *FS) History(path string) ([]Change, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	nd, err := fs.lkr.LookupModNode(path)
	if err != nil {
		return nil, err
	}

	status, err := fs.lkr.Status()
	if err != nil {
		return nil, err
	}

	hist, err := vcs.History(fs.lkr, nd, status, nil)
	if err != nil {
		return nil, err
	}

	hashToRef, err := fs.buildCommitHashToRefTable()
	if err != nil {
		return nil, err
	}

	entries := []Change{}
	for _, change := range hist {
		head := &Commit{
			Hash: change.Head.Hash().Clone(),
			Msg:  change.Head.Message(),
			Tags: hashToRef[change.Head.Hash().B58String()],
			Date: change.Head.ModTime(),
		}

		var next *Commit
		if change.Next != nil {
			next = &Commit{
				Hash: change.Next.Hash().Clone(),
				Msg:  change.Next.Message(),
				Tags: hashToRef[change.Next.Hash().B58String()],
				Date: change.Next.ModTime(),
			}
		}

		entries = append(entries, Change{
			Path:    change.Curr.Path(),
			Change:  change.Mask.String(),
			Head:    head,
			Next:    next,
			ReferTo: change.ReferToPath,
		})
	}

	return entries, nil
}

// Sync will synchronize the state of two filesystems.
// If one of filesystems have unstaged changes, they will be committted first.
// If our filesystem was changed by Sync(), a new merge commit will also be created.
func (fs *FS) Sync(remote *FS) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	doPinOrUnpin := func(doPin, explicit bool, nd n.ModNode) {
		file, ok := nd.(*n.File)
		if !ok {
			// Non-files are simply ignored.
			return
		}

		op, opName := fs.bk.Unpin, "unpin"
		if doPin {
			op, opName = fs.bk.Pin, "pin"
		}

		if err := op(file.Content(), explicit); err != nil {
			log.Warningf("Failed to %s hash: %v", opName, file.Content())
		}
	}

	// Make sure we pin/unpin files correctly after the sync:
	syncCfg := &fs.cfg.sync
	syncCfg.OnAdd = func(newNd n.ModNode) bool {
		doPinOrUnpin(true, false, newNd)
		return true
	}
	syncCfg.OnRemove = func(oldNd n.ModNode) bool {
		doPinOrUnpin(false, true, oldNd)
		return true
	}
	syncCfg.OnMerge = func(newNd, oldNd n.ModNode) bool {
		_, isExplicit, err := fs.bk.IsPinned(oldNd.Content())
		if err != nil {
			log.Warnf(
				"failed to check pin status of old node `%s` (%v)",
				oldNd.Path(),
				oldNd.Content(),
			)
		}

		// Pin new node with old pin state:
		doPinOrUnpin(true, isExplicit, newNd)
		doPinOrUnpin(false, true, oldNd)
		return true
	}
	syncCfg.OnConflict = func(src, dst n.ModNode) bool {
		// Don't need to do something,
		// conflict file will not get a pin by default.
		// TODO: Does this make sense?
		return true
	}

	return vcs.Sync(remote.lkr, fs.lkr, syncCfg)
}

func (fs *FS) MakeDiff(remote *FS, headRevOwn, headRevRemote string) (*Diff, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	srcHead, err := parseRev(remote.lkr, headRevRemote)
	if err != nil {
		return nil, e.Wrapf(err, "parse remote ref")
	}

	dstHead, err := parseRev(fs.lkr, headRevOwn)
	if err != nil {
		return nil, e.Wrapf(err, "parse own ref")
	}

	realDiff, err := vcs.MakeDiff(remote.lkr, fs.lkr, srcHead, dstHead, &fs.cfg.sync)
	if err != nil {
		return nil, e.Wrapf(err, "make diff")
	}

	// "fake" is the diff that we give to the outside.
	// Internally we have a bit more knowledge.
	fakeDiff := &Diff{}

	// Convert the simple slice parts:
	for _, nd := range realDiff.Added {
		fakeDiff.Added = append(fakeDiff.Added, *fs.nodeToStat(nd))
	}

	for _, nd := range realDiff.Ignored {
		fakeDiff.Ignored = append(fakeDiff.Added, *fs.nodeToStat(nd))
	}

	for _, nd := range realDiff.Removed {
		fakeDiff.Removed = append(fakeDiff.Removed, *fs.nodeToStat(nd))
	}

	for _, nd := range realDiff.Missing {
		fakeDiff.Missing = append(fakeDiff.Missing, *fs.nodeToStat(nd))
	}

	// And also convert the slightly more complex pairs:
	for _, pair := range realDiff.Moved {
		fakeDiff.Moved = append(fakeDiff.Moved, DiffPair{
			Src: *fs.nodeToStat(pair.Src),
			Dst: *fs.nodeToStat(pair.Dst),
		})
	}

	for _, pair := range realDiff.Merged {
		fakeDiff.Merged = append(fakeDiff.Merged, DiffPair{
			Src: *fs.nodeToStat(pair.Src),
			Dst: *fs.nodeToStat(pair.Dst),
		})
	}

	for _, pair := range realDiff.Conflict {
		fakeDiff.Conflict = append(fakeDiff.Conflict, DiffPair{
			Src: *fs.nodeToStat(pair.Src),
			Dst: *fs.nodeToStat(pair.Dst),
		})
	}

	return fakeDiff, nil
}

func (fs *FS) buildCommitHashToRefTable() (map[string][]string, error) {
	names, err := fs.lkr.ListRefs()
	if err != nil {
		return nil, err
	}

	hashToRef := make(map[string][]string)
	for _, name := range names {
		cmt, err := fs.lkr.ResolveRef(name)
		if err != nil {
			return nil, err
		}

		if cmt != nil {
			key := cmt.Hash().B58String()
			hashToRef[key] = append(hashToRef[key], name)
		}
	}

	return hashToRef, nil
}

// Log returns a list of commits starting with the staging commit until the
// initial commit. For each commit, metadata is collected.
func (fs *FS) Log() ([]Commit, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	hashToRef, err := fs.buildCommitHashToRefTable()
	if err != nil {
		return nil, err
	}

	entries := []Commit{}
	return entries, c.Log(fs.lkr, func(cmt *n.Commit) error {
		entries = append(entries, Commit{
			Hash: cmt.Hash().Clone(),
			Msg:  cmt.Message(),
			Tags: hashToRef[cmt.Hash().B58String()],
			Date: cmt.ModTime(),
		})

		return nil
	})
}

func (fs *FS) Reset(path, rev string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if path == "/" {
		return fs.Checkout(rev, false)
	}

	cmt, err := parseRev(fs.lkr, rev)
	if err != nil {
		return err
	}

	if err := fs.lkr.CheckoutFile(cmt, path); err != nil {
		return err
	}

	// Cannot (un)pin non-existing file anymore.
	if _, err := fs.lkr.LookupNode(path); ie.IsNoSuchFileError(err) {
		return nil
	}

	if err := fs.pinOp(path, false, fs.bk.Unpin); err != nil {
		return err
	}

	return fs.pinOp(path, false, fs.bk.Pin)
}

func (fs *FS) Checkout(rev string, force bool) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	cmt, err := parseRev(fs.lkr, rev)
	if err != nil {
		return err
	}

	return fs.lkr.CheckoutCommit(cmt, force)
}

// Tag saves a human readable name for the revision pointed to by `rev`.
// There are three pre-defined tags available:
//
// - HEAD: The last full commit.
// - CURR: The current commit (== staging commit)
// - INIT: the initial commit.
//
// The tagname is case-insensitive.
func (fs *FS) Tag(rev, name string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	cmt, err := parseRev(fs.lkr, rev)
	if err != nil {
		return err
	}

	return fs.lkr.SaveRef(name, cmt)
}

// RemoveTag removes a previously created tag.
func (fs *FS) RemoveTag(name string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	return fs.lkr.RemoveRef(name)
}

func (fs *FS) FilesByContent(contents []h.Hash) (map[string]StatInfo, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	files, err := fs.lkr.FilesByContents(contents)
	if err != nil {
		return nil, err
	}

	infos := make(map[string]StatInfo)
	for content, file := range files {
		infos[content] = *fs.nodeToStat(file)
	}

	return infos, nil
}

func (fs *FS) ScheduleGCRun() {
	// Putting a value into gcControl might block,
	// so better do it in the background.
	go func() {
		fs.gcControl <- true
	}()
}
