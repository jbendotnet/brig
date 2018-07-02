package catfs

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"sort"
	"testing"
	"time"

	log "github.com/Sirupsen/logrus"
	c "github.com/sahib/brig/catfs/core"
	ie "github.com/sahib/brig/catfs/errors"
	"github.com/sahib/brig/catfs/mio"
	"github.com/sahib/brig/catfs/mio/chunkbuf"
	"github.com/sahib/brig/catfs/mio/compress"
	n "github.com/sahib/brig/catfs/nodes"
	"github.com/sahib/config"
	"github.com/sahib/brig/defaults"
	h "github.com/sahib/brig/util/hashlib"
	"github.com/sahib/brig/util/testutil"
	"github.com/stretchr/testify/require"
)

func init() {
	log.SetLevel(log.WarnLevel)
}

func withDummyFS(t *testing.T, fn func(fs *FS)) {
	backend := NewMemFsBackend()
	owner := "alice"

	dbPath, err := ioutil.TempDir("", "brig-fs-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	defer func() {
		if err := os.RemoveAll(dbPath); err != nil {
			t.Fatalf("Failed to clean up %s: %v", dbPath, err)
		}
	}()

	cfg, err := config.Open(nil, defaults.Defaults)
	require.Nil(t, err)

	fsCfg := cfg.Section("fs")

	fs, err := NewFilesystem(backend, dbPath, owner, fsCfg)
	if err != nil {
		t.Fatalf("Failed to create filesystem: %v", err)
	}

	fn(fs)

	if err := fs.Close(); err != nil {
		t.Fatalf("Failed to close filesystem: %v", err)
	}
}

func TestStat(t *testing.T) {
	t.Parallel()

	withDummyFS(t, func(fs *FS) {
		_, err := fs.Stat("/sub/x")
		require.True(t, ie.IsNoSuchFileError(err))

		c.MustMkdir(t, fs.lkr, "/sub")
		file := c.MustTouch(t, fs.lkr, "/sub/x", 1)

		info, err := fs.Stat("/sub/x")
		require.Nil(t, err)
		require.Equal(t, info.Path, "/sub/x")
		require.Equal(t, info.IsDir, false)
		require.Equal(t, info.Size, uint64(0))
		require.Equal(t, info.Inode, file.Inode())
		require.Equal(t, info.TreeHash, file.TreeHash())

		file.SetSize(42)
		require.Nil(t, fs.lkr.StageNode(file))

		info, err = fs.Stat("/sub/x")
		require.Nil(t, err)
		require.Equal(t, info.Size, uint64(42))
		require.Equal(t, info.TreeHash, file.TreeHash())

		info, err = fs.Stat("/sub")
		require.Nil(t, err)
		require.Equal(t, info.Path, "/sub")
		require.Equal(t, info.IsDir, true)
		// TODO:
		// require.Equal(t, info.Size, uint64(42))
	})
}

func TestLogAndTag(t *testing.T) {
	t.Parallel()

	withDummyFS(t, func(fs *FS) {
		cmts := []*n.Commit{}
		for idx := 0; idx < 10; idx++ {
			_, cmt := c.MustTouchAndCommit(t, fs.lkr, "/x", byte(idx))

			hash := cmt.TreeHash().B58String()
			if err := fs.Tag(hash, fmt.Sprintf("tag%d", idx)); err != nil {
				t.Fatalf("Failed to tag %v: %v", hash, err)
			}

			cmts = append(cmts, cmt)
		}

		status, err := fs.lkr.Status()
		require.Nil(t, err)

		cmts = append(cmts, status)

		log, err := fs.Log()
		require.Nil(t, err)

		for idx, entry := range log {
			ridx := len(cmts) - idx - 1
			cmt := cmts[ridx]
			require.Equal(t, entry.Hash, cmt.TreeHash())

			msg := fmt.Sprintf("cmt %d", ridx)
			tags := []string{fmt.Sprintf("tag%d", ridx)}

			// 0 is status, 1 is head, 10 is initial
			switch idx {
			case 0:
				tags = []string{"curr"}
				msg = ""
			case 1:
				tags = append(tags, "head")
			case 10:
				tags = append(tags, "init")
			}

			sort.Sort(sort.Reverse(sort.StringSlice(entry.Tags)))
			require.EqualValues(t, tags, entry.Tags)
			require.Equal(t, entry.Msg, msg)
		}
	})
}

var TestKey = []byte("01234567890ABCDE01234567890ABCDE")

func TestCat(t *testing.T) {
	t.Parallel()

	withDummyFS(t, func(fs *FS) {
		raw := []byte{1, 2, 3}
		rinRaw := bytes.NewBuffer(raw)

		rin, err := mio.NewInStream(rinRaw, TestKey, compress.AlgoSnappy)
		require.Nil(t, err)

		backendHash, err := fs.bk.Add(rin)
		require.Nil(t, err)

		contentHash := h.TestDummy(t, 23)

		// Stage the file manually (without fs.Stage)
		_, err = c.Stage(fs.lkr, "/x", contentHash, backendHash, uint64(len(raw)), TestKey)
		require.Nil(t, err)

		// Cat the file again:
		stream, err := fs.Cat("/x")
		require.Nil(t, err)

		// Check if the returned stream really contains 1,2,3
		result := bytes.NewBuffer(nil)
		_, err = stream.WriteTo(result)
		require.Nil(t, err)
		require.Equal(t, result.Bytes(), raw)
	})
}

func TestStage(t *testing.T) {
	t.Parallel()

	tcs := [][]byte{
		{},
		{1},
		{1, 2, 3},
		testutil.CreateDummyBuf(8 * 1024),
	}

	for idx, tc := range tcs {
		t.Run(fmt.Sprintf("%d", idx), func(t *testing.T) {
			withDummyFS(t, func(fs *FS) {
				buf := chunkbuf.NewChunkBuffer(tc)
				require.Nil(t, fs.Stage("/x", buf))

				stream, err := fs.Cat("/x")
				require.Nil(t, err)

				data, err := ioutil.ReadAll(stream)
				require.Nil(t, err)

				require.Equal(t, data, tc)
				require.Nil(t, stream.Close())

				file, err := fs.lkr.LookupFile("/x")
				require.Nil(t, err)

				key := file.Key()
				oldKey := make([]byte, len(key))
				copy(oldKey, key)

				// Also insert some more data to modify an existing file.
				nextData := []byte{6, 6, 6, 6, 6, 6}
				require.Nil(t, fs.Stage("/x", chunkbuf.NewChunkBuffer((nextData))))
				stream, err = fs.Cat("/x")
				require.Nil(t, err)
				data, err = ioutil.ReadAll(stream)
				require.Nil(t, err)
				require.Equal(t, data, nextData)
				require.Nil(t, stream.Close())

				// Check that the key did not change during modifying an existing file.
				file, err = fs.lkr.LookupFile("/x")
				require.Nil(t, err)
				require.Equal(t, file.Key(), oldKey)
			})
		})
	}
}

func TestHistory(t *testing.T) {
	t.Parallel()

	withDummyFS(t, func(fs *FS) {
		require.Nil(t, fs.MakeCommit("hello"))
		require.Nil(t, fs.Stage("/x", chunkbuf.NewChunkBuffer([]byte{1})))
		require.Nil(t, fs.MakeCommit("1"))
		require.Nil(t, fs.Stage("/x", chunkbuf.NewChunkBuffer([]byte{2})))
		require.Nil(t, fs.MakeCommit("2"))
		require.Nil(t, fs.Stage("/x", chunkbuf.NewChunkBuffer([]byte{3})))
		require.Nil(t, fs.MakeCommit("3"))

		hist, err := fs.History("/x")
		require.Nil(t, err)

		log, err := fs.Log()
		require.Nil(t, err)

		for idx, entry := range hist {
			require.Equal(t, entry.Path, "/x")

			change := "none"
			switch idx {
			case 1, 2:
				change = "modified"
			case 3:
				change = "added"
			}

			require.Equal(t, entry.Change, change)
			require.Equal(
				t,
				log[idx].Hash.B58String(),
				entry.Head.Hash.B58String(),
			)
		}
	})
}

func mustReadPath(t *testing.T, fs *FS, path string) []byte {
	stream, err := fs.Cat(path)
	require.Nil(t, err)

	data, err := ioutil.ReadAll(stream)
	require.Nil(t, err)

	return data
}

func TestReset(t *testing.T) {
	t.Parallel()

	withDummyFS(t, func(fs *FS) {
		require.Nil(t, fs.MakeCommit("hello"))

		require.Nil(t, fs.Stage("/x", chunkbuf.NewChunkBuffer([]byte{1})))
		require.Nil(t, fs.MakeCommit("1"))

		// Modify on stage:
		require.Nil(t, fs.Stage("/x", chunkbuf.NewChunkBuffer([]byte{2})))
		require.Nil(t, fs.Reset("/x", "HEAD"))

		data := mustReadPath(t, fs, "/x")
		require.Equal(t, data[0], byte(1))
		if err := fs.MakeCommit("2"); err != ie.ErrNoChange {
			t.Fatalf("Reset did clearly not reset stuff... (something changed)")
		}

		// Remove the file and then reset it (like git checkout -- file)
		require.Nil(t, fs.Remove("/x"))
		if _, err := fs.Cat("/x"); !ie.IsNoSuchFileError(err) {
			t.Fatalf("Something wrong with removed node")
		}

		// Check if we can recover the delete:
		require.Nil(t, fs.Reset("/x", "HEAD"))
		data = mustReadPath(t, fs, "/x")
		require.Equal(t, data[0], byte(1))

		// Reset to something non-existing -> error.
		require.NotNil(t, fs.Reset("/x", "DEADBEEF"))

		// Reset to the very first commit - node did not exist back then.
		require.Nil(t, fs.Reset("/x", "INIT"))

		// Should not exist anymore currently.
		_, err := fs.Stat("/x")
		require.True(t, ie.IsNoSuchFileError(err))
	})
}

func TestCheckout(t *testing.T) {
	t.Parallel()

	withDummyFS(t, func(fs *FS) {
		require.Nil(t, fs.MakeCommit("hello"))
		hello, err := fs.Head()
		require.Nil(t, err)

		require.Nil(t, fs.Touch("/x"))
		require.Nil(t, fs.Touch("/y"))
		require.Nil(t, fs.Touch("/z"))
		require.Nil(t, fs.Stage("/x", bytes.NewReader([]byte{1, 2, 3})))

		require.Nil(t, fs.Remove("/y"))
		require.Nil(t, fs.Move("/z", "/a"))

		require.Nil(t, fs.MakeCommit("world"))
		world, err := fs.Head()
		require.Nil(t, err)

		require.Nil(t, fs.Touch("/new"))
		require.Nil(t, fs.Stage("/x", bytes.NewReader([]byte{4, 5, 6})))

		err = fs.Checkout(world, false)
		require.Equal(t, err, ie.ErrStageNotEmpty)

		err = fs.Checkout(world, true)
		require.Nil(t, err)

		_, err = fs.Stat("/new")
		require.True(t, ie.IsNoSuchFileError(err))

		xStream, err := fs.Cat("/x")
		require.Nil(t, err)
		data, err := ioutil.ReadAll(xStream)
		require.Nil(t, err)
		require.Equal(t, data, []byte{1, 2, 3})

		err = fs.Checkout(hello, true)
		require.Nil(t, err)

		_, err = fs.Stat("/x")
		require.True(t, ie.IsNoSuchFileError(err))
	})
}

func TestExportImport(t *testing.T) {
	t.Parallel()

	withDummyFS(t, func(fs *FS) {
		require.Nil(t, fs.MakeCommit("hello world"))

		// Add a single file:
		buf := chunkbuf.NewChunkBuffer([]byte{1, 2, 3})
		require.Nil(t, fs.Stage("/x", buf))
		require.Nil(t, fs.MakeCommit("touchy touchy"))

		// Stage something to see if this will also be exported
		// (it most defintely should)
		buf = chunkbuf.NewChunkBuffer([]byte{3, 2, 1})
		require.Nil(t, fs.Stage("/x", buf))

		mem := &bytes.Buffer{}
		require.Nil(t, fs.Export(mem))

		// Check if we can import all this data:
		withDummyFS(t, func(newFs *FS) {
			require.Nil(t, fs.Import(mem))

			stream, err := fs.Cat("/x")
			require.Nil(t, err)

			data, err := ioutil.ReadAll(stream)
			require.Nil(t, err)
			require.Equal(t, []byte{3, 2, 1}, data)
		})
	})
}

func TestSync(t *testing.T) {
	t.Parallel()

	// There are a lot more tests in vcs/*
	// This is only a test to see if the high-level api is working.
	withDummyFS(t, func(fsa *FS) {
		require.Nil(t, fsa.MakeCommit("hello a"))
		withDummyFS(t, func(fsb *FS) {
			require.Nil(t, fsb.MakeCommit("hello b"))
			require.Nil(t, fsa.Sync(fsb))

			require.Nil(t, fsb.Stage("/x", bytes.NewReader([]byte{1})))
			require.Nil(t, fsb.Stage("/y", bytes.NewReader([]byte{2})))
			require.Nil(t, fsb.Stage("/z", bytes.NewReader([]byte{3})))

			// Actually sync the results:
			require.Nil(t, fsa.Sync(fsb))

			info, err := fsa.Stat("/x")
			require.Nil(t, err)
			require.Equal(t, info.Path, "/x")

			info, err = fsa.Stat("/y")
			require.Nil(t, err)
			require.Equal(t, info.Path, "/y")

			info, err = fsa.Stat("/z")
			require.Nil(t, err)
			require.Equal(t, info.Path, "/z")
		})
	})
}

func TestMakeDiff(t *testing.T) {
	t.Parallel()

	// There are a lot more tests in vcs/*
	// This is only a test for the high-level api.
	withDummyFS(t, func(fsa *FS) {
		fsaX := c.MustTouch(t, fsa.lkr, "/x", 1)
		fsaY := c.MustTouch(t, fsa.lkr, "/y", 2)
		fsaZ := c.MustTouch(t, fsa.lkr, "/z", 3)

		require.Nil(t, fsa.MakeCommit("hello a"))
		withDummyFS(t, func(fsb *FS) {
			require.Nil(t, fsb.MakeCommit("hello b"))
			require.Nil(t, fsa.Sync(fsb))

			fsbX := c.MustTouch(t, fsb.lkr, "/x", 4)
			c.MustTouch(t, fsb.lkr, "/y", 5)
			fsbZ := c.MustTouch(t, fsb.lkr, "/z", 6)
			fsbA := c.MustTouch(t, fsb.lkr, "/a", 7)

			require.Nil(t, fsb.MakeCommit("stuff"))
			require.Nil(t, fsb.Remove("/y"))
			require.Nil(t, fsb.MakeCommit("before diff"))

			// Use the upwards notation:
			diff, err := fsa.MakeDiff(fsb, "head^^^", "curr")
			require.Nil(t, err)

			require.Equal(t, []StatInfo{*fsb.nodeToStat(fsbA)}, diff.Added)
			require.Equal(t, []StatInfo{*fsa.nodeToStat(fsaY)}, diff.Removed)
			require.Equal(t, []DiffPair{{
				Src: *fsb.nodeToStat(fsbX),
				Dst: *fsa.nodeToStat(fsaX),
			}, {
				Src: *fsb.nodeToStat(fsbZ),
				Dst: *fsa.nodeToStat(fsaZ),
			}}, diff.Conflict)
		})
	})
}

func TestPin(t *testing.T) {
	t.Parallel()

	withDummyFS(t, func(fs *FS) {
		// TODO: what happens if we have two files with the same content?
		require.Nil(t, fs.Stage("/x", bytes.NewReader([]byte{1})))
		require.Nil(t, fs.Stage("/y", bytes.NewReader([]byte{2})))

		require.Nil(t, fs.Unpin("/x"))
		require.Nil(t, fs.Unpin("/y"))

		isPinned, isExplicit, err := fs.IsPinned("/x")
		require.Nil(t, err)
		require.False(t, isPinned)
		require.False(t, isExplicit)

		require.Nil(t, fs.Pin("/x"))

		isPinned, isExplicit, err = fs.IsPinned("/x")
		require.Nil(t, err)
		require.True(t, isPinned)
		require.True(t, isExplicit)

		isPinned, isExplicit, err = fs.IsPinned("/")
		require.Nil(t, err)
		require.False(t, isPinned)
		require.False(t, isExplicit)

		require.Nil(t, fs.Pin("/"))

		isPinned, isExplicit, err = fs.IsPinned("/")
		require.Nil(t, err)
		require.True(t, isPinned)
		require.True(t, isExplicit)

		require.Nil(t, fs.Unpin("/"))

		isPinned, isExplicit, err = fs.IsPinned("/")
		require.Nil(t, err)
		require.False(t, isPinned)
		require.False(t, isExplicit)

		isPinned, isExplicit, err = fs.IsPinned("/x")
		require.Nil(t, err)
		require.False(t, isPinned)
		require.False(t, isExplicit)
	})
}

func TestMkdir(t *testing.T) {
	t.Parallel()

	withDummyFS(t, func(fs *FS) {
		err := fs.Mkdir("/a/b/c/d", false)
		require.True(t, ie.IsNoSuchFileError(err))

		_, err = fs.Stat("/a")
		require.True(t, ie.IsNoSuchFileError(err))

		err = fs.Mkdir("/a/b/c/d", true)
		require.Nil(t, err)

		info, err := fs.Stat("/a")
		require.Nil(t, err)
		require.True(t, info.IsDir)

		// Check that it still works if the directory exists
		err = fs.Mkdir("/a/b/c/d", false)
		require.Nil(t, err)

		err = fs.Mkdir("/a/b/c/d", true)
		require.Nil(t, err)

		err = fs.Mkdir("/a/b/c", false)
		require.Nil(t, err)
	})
}

func TestMove(t *testing.T) {
	t.Parallel()

	withDummyFS(t, func(fs *FS) {
		require.Nil(t, fs.Touch("/x"))
		require.Nil(t, fs.Move("/x", "/y"))

		_, err := fs.Stat("/x")
		require.True(t, ie.IsNoSuchFileError(err))

		info, err := fs.Stat("/y")
		require.Nil(t, err)
		require.Equal(t, info.Path, "/y")
		require.False(t, info.IsDir)
	})
}

func TestTouch(t *testing.T) {
	t.Parallel()

	withDummyFS(t, func(fs *FS) {
		require.Nil(t, fs.Touch("/x"))
		oldInfo, err := fs.Stat("/x")
		require.Nil(t, err)

		require.Nil(t, fs.Stage("/x", bytes.NewReader([]byte{1, 2, 3})))

		require.Nil(t, fs.Touch("/x"))
		newInfo, err := fs.Stat("/x")
		require.Nil(t, err)

		// Check that the timestamp advanced only.
		require.True(t, oldInfo.ModTime.Before(newInfo.ModTime))

		// Also check that the content was not deleted:
		stream, err := fs.Cat("/x")
		require.Nil(t, err)

		data, err := ioutil.ReadAll(stream)
		require.Nil(t, err)
		require.Equal(t, data, []byte{1, 2, 3})

		require.Nil(t, stream.Close())
	})
}

func TestHead(t *testing.T) {
	t.Parallel()

	withDummyFS(t, func(fs *FS) {
		_, err := fs.Head()

		require.True(t, ie.IsErrNoSuchRef(err))
		require.Nil(t, fs.MakeCommit("init"))

		ref, err := fs.Head()
		require.Nil(t, err)

		headCmt, err := fs.lkr.ResolveRef("HEAD")
		require.Nil(t, err)

		require.Equal(t, headCmt.TreeHash().B58String(), ref)
	})
}

func TestList(t *testing.T) {
	t.Parallel()

	withDummyFS(t, func(fs *FS) {
		require.Nil(t, fs.Touch("/x"))
		require.Nil(t, fs.Mkdir("/1/2/3/", true))
		require.Nil(t, fs.Touch("/1/2/3/y"))

		entries, err := fs.List("/1/2", -1)
		require.Nil(t, err)

		require.Equal(t, len(entries), 3)
		require.Equal(t, entries[0].Path, "/1/2")
		require.Equal(t, entries[1].Path, "/1/2/3")
		require.Equal(t, entries[2].Path, "/1/2/3/y")

		entries, err = fs.List("/", 1)
		require.Nil(t, err)

		require.Equal(t, 2, len(entries))
		require.Equal(t, entries[0].Path, "/1")
		require.Equal(t, entries[1].Path, "/x")

		dir, err := fs.lkr.LookupDirectory("/1")
		require.Nil(t, err)

		// Check if ghosts are being treated as not existant:
		c.MustMove(t, fs.lkr, dir, "/666")
		_, err = fs.List("/1", -1)
		require.True(t, ie.IsNoSuchFileError(err))
		_, err = fs.List("/666", -1)
		require.Nil(t, err)
	})
}

func TestTag(t *testing.T) {
	t.Parallel()

	withDummyFS(t, func(fs *FS) {
		require.Nil(t, fs.Touch("/x"))
		require.Nil(t, fs.MakeCommit("init"))

		head, err := fs.Head()
		require.Nil(t, err)

		// try with an abbreviated tag name.
		require.Nil(t, fs.Tag(head[:10], "xxx"))
		cmt, err := fs.lkr.ResolveRef("xxx")
		require.Nil(t, err)
		require.Equal(t, cmt.(*n.Commit).Message(), "init")

		require.Nil(t, fs.RemoveTag("xxx"))
		cmt, err = fs.lkr.ResolveRef("xxx")
		require.Nil(t, cmt)
		require.True(t, ie.IsErrNoSuchRef(err))
	})
}

func TestStageUnmodified(t *testing.T) {
	t.Parallel()

	withDummyFS(t, func(fs *FS) {
		require.Nil(t, fs.Stage("/x", bytes.NewReader([]byte{1})))
		infoOld, err := fs.Stat("/x")
		require.Nil(t, err)

		// Just to be sure:
		time.Sleep(50 * time.Millisecond)

		require.Nil(t, fs.Stage("/x", bytes.NewReader([]byte{1})))
		infoNew, err := fs.Stat("/x")
		require.Nil(t, err)

		require.Equal(t, infoOld.ModTime, infoNew.ModTime)
	})
}

func TestTruncate(t *testing.T) {
	t.Parallel()

	withDummyFS(t, func(fs *FS) {
		data := testutil.CreateDummyBuf(1024)
		require.Nil(t, fs.Stage("/x", bytes.NewReader(data)))

		for _, size := range []int{1025, 512, 1, 0, 1024} {
			t.Run(fmt.Sprintf("size-%d", size), func(t *testing.T) {
				require.Nil(t, fs.Truncate("/x", uint64(size)))
				// clamp to 1024 for assertion:
				readSize := size
				if size > 1024 {
					readSize = 1024
				}

				stream, err := fs.Cat("/x")
				require.Nil(t, err)
				readData, err := ioutil.ReadAll(stream)
				require.Nil(t, err)
				require.Equal(t, len(readData), readSize)
				require.Equal(t, readData, data[:readSize])
			})
		}

		require.NotNil(t, fs.Truncate("/", 0))
	})
}

func TestChangingCompressAlgos(t *testing.T) {
	t.Parallel()

	withDummyFS(t, func(fs *FS) {
		// Create a file which will not be compressed.
		oldData := testutil.CreateDummyBuf(compress.HeaderSizeThreshold - 1)
		require.Nil(t, fs.Stage("/a-text-file.go", bytes.NewReader(oldData)))

		// Second run will use another compress algorithm, since we're
		// over the header size limit in the compression guesser.
		newData := testutil.CreateDummyBuf(compress.HeaderSizeThreshold + 1)
		require.Nil(t, fs.Stage("/a-text-file.go", bytes.NewReader(newData)))

		stream, err := fs.Cat("/a-text-file.go")
		require.Nil(t, err)

		gotData, err := ioutil.ReadAll(stream)
		require.Nil(t, err)

		require.Equal(t, newData, gotData)
	})
}

func TestPatch(t *testing.T) {
	withDummyFS(t, func(srcFs *FS) {
		withDummyFS(t, func(dstFs *FS) {
			require.Nil(t, srcFs.MakeCommit("init"))
			require.Nil(t, srcFs.Touch("/x"))
			require.Nil(t, srcFs.MakeCommit("added x"))

			srcIndex, err := srcFs.LastPatchIndex()
			require.Nil(t, err)
			require.Equal(t, int64(0), srcIndex)

			dstIndex, err := dstFs.LastPatchIndex()
			require.Nil(t, err)
			require.Equal(t, int64(0), dstIndex)

			patch, err := srcFs.MakePatch("commit[0]", nil)
			require.Nil(t, err)

			require.Nil(t, dstFs.ApplyPatch(patch))
			srcX, err := srcFs.Stat("/x")
			require.Nil(t, err)

			srcIndex, err = srcFs.LastPatchIndex()
			require.Nil(t, err)
			require.Equal(t, int64(0), srcIndex)

			dstX, err := dstFs.Stat("/x")
			require.Nil(t, err)

			require.Equal(t, srcX.Path, dstX.Path)
			require.Equal(t, srcX.Size, dstX.Size)
			require.Equal(t, srcX.ContentHash, dstX.ContentHash)
			require.Equal(t, srcX.BackendHash, dstX.BackendHash)

			dstIndex, err = dstFs.LastPatchIndex()
			require.Nil(t, err)
			require.Equal(t, int64(2), dstIndex)
		})
	})
}
