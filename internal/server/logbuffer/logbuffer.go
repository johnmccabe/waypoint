// Package logbuffer provides a structure and API for efficiently reading
// and writing logs that may be streamed to a server.
package logbuffer

import (
	"context"
	"sync"
	"sync/atomic"

	pb "github.com/mitchellh/devflow/internal/server/gen"
)

// Entry is an alias to our entry type. We do this to simplify the typing.
type Entry = pb.LogBatch_Entry

var (
	chunkCount = 32
	chunkSize  = 512
)

// Buffer is a data structure for buffering logs with concurrent read/write access.
//
// Callers can use easy APIs to write and read data and the storage and access
// is managed underneath. If a reader falls behind a writer significantly, then
// the next read may "jump" forward to catch up. There is no way to explicitly
// detect a jump currently.
//
// Writer
//
// The writer calls Write on the buffer as it gets log entries. Multiple
// writers are safe to use. The buffer will always successfully write all
// entries, though it may result in extra allocations.
//
//     buf.Write(entries...)
//
// Reader
//
// A reader calls buf.Reader to get a Reader structure, and then calls Read
// to read values from the buffer. The Reader structure is used to maintain
// per-reader cursors so that multiple readers can exist at multiple points.
//
// Internal Details
//
// A buffer is structured as a sliding window over a set of "chunks". A chunk
// is a set of log entries. As you write into the buffer, the buffer will
// append to the current chunk until it is full, then move to the next chunk.
// When all the chunks are full, the buffer will allocate a new set of chunks.
//
// The break into "chunks" is done for two reasons. The first is to prevent
// overallocation; we don't need to allocate a lot of buffer space, only enough
// for the current chunk. Second, to avoid lock contention. Once a chunk is
// full, it will never be written to again so we never need to acquire a lock
// to read the data. This makes reading backlogs very fast.
type Buffer struct {
	chunks  []chunk
	cond    *sync.Cond
	current int
}

// New creates a new Buffer.
func New() *Buffer {
	var m sync.Mutex
	return &Buffer{
		chunks: make([]chunk, chunkCount),
		cond:   sync.NewCond(&m),
	}
}

// Write writes the set of entries into the buffer.
//
// This is safe for concurrent access.
func (b *Buffer) Write(entries ...*Entry) {
	b.cond.L.Lock()
	defer b.cond.L.Unlock()

	// Write all our entries
	for n := 0; n < len(entries); {
		current := &b.chunks[b.current]

		// Write our entries
		n += current.write(entries[n:])

		// If our chunk is full, we need to move to the next chunk or
		// otherwise move the full window.
		if current.full() {
			b.current++ // move to the next chunk

			// If our index is beyond the end of our chunk list then we
			// allocate a new chunk list and move to that. Existing readers
			// hold on to the reference to the old chunk list so they'll
			// finish reading there.
			if b.current >= len(b.chunks) {
				b.chunks = make([]chunk, chunkCount)
				b.current = 0
			}
		}
	}

	// Wake up any sleeping readers
	b.cond.Broadcast()
}

// Reader returns a shared reader for this buffer. The Reader provides
// an easy-to-use API to read log entries.
func (b *Buffer) Reader() *Reader {
	b.cond.L.Lock()
	defer b.cond.L.Unlock()
	return &Reader{b: b, chunks: b.chunks, closeCh: make(chan struct{})}
}

// Reader reads log entry values from a buffer.
//
// Each Reader maintains its own read cursor. This allows multiple readers
// to exist across a Buffer at multiple points. Subsequent calls to Read
// may "jump" across time if a reader falls behind the writer.
//
// It is not safe to call Read concurrently. If you want concurrent read
// access you can either create multiple readers or protect Read with a lock.
// You may call Close concurrently with Read.
type Reader struct {
	b       *Buffer
	chunks  []chunk
	closeCh chan struct{}
	idx     int
	cursor  uint32
	closed  uint32
}

// Read returns a batch of log entries, up to "max" amount. If "max" isn't
// available, this will return any number that currently exists. If zero
// exist, this will block waiting for available entries.
func (r *Reader) Read(max int) []*Entry {
	// If we're closed then do nothing.
	if atomic.LoadUint32(&r.closed) > 0 {
		return nil
	}

	chunk := &r.chunks[r.idx] // Important: this must be the pointer
	result, cursor := chunk.read(r.b.cond, &r.closed, r.cursor, uint32(max))

	// If we're not at the end, return our result
	if !chunk.atEnd(cursor) {
		r.cursor = cursor
		return result
	}

	// We're at the end of this chunk, move to the next one
	r.idx++
	r.cursor = 0

	// If we're at the end of our chunk list, get the next set
	if r.idx >= len(r.chunks) {
		r.chunks = r.b.Reader().chunks
		r.idx = 0
	}

	return result
}

// Close closes the reader. This will cause all future Read calls to
// return immediately with a nil result. This will also immediately unblock
// any currently blocked Reads.
//
// This is safe to call concurrently with Read.
func (r *Reader) Close() error {
	if atomic.CompareAndSwapUint32(&r.closed, 0, 1) {
		close(r.closeCh)

		// Only broadcast if we closed. The broadcast will wake up any waiters
		// which will see that the reader is closed.
		r.b.cond.Broadcast()
	}

	return nil
}

// CloseContext will block until ctx is done and then close the reader.
// This can be called multiple times to register multiple context values
// to close the reader on.
func (r *Reader) CloseContext(ctx context.Context) {
	select {
	case <-ctx.Done():
		r.Close()

	case <-r.closeCh:
		// Someone else closed, exit.
	}
}

type chunk struct {
	idx    uint32
	buffer []*Entry
}

// atEnd returns true if the cursor is at the end of the chunk. The
// end means that there will never be any more new values.
func (w *chunk) atEnd(cursor uint32) bool {
	return cursor > 0 && cursor >= uint32(len(w.buffer))
}

// full returns true if this chunk is full. full means that the write
// cursor is at the end of the chunk and no more data can be written. Any
// calls to write will return with 0.
func (w *chunk) full() bool {
	return w.atEnd(atomic.LoadUint32(&w.idx))
}

// read reads up to max number of elements from the chunk from the current
// cursor value. If any values are available, this will return up to max
// amount immediately. If no values are available, this will block until
// more become available.
//
// The caller should take care to check chunk.atEnd with their cursor to
// see if they're at the end of the chunk. If you're at the end of the chunk,
// this will always return immediately to avoid blocking forever.
func (w *chunk) read(cond *sync.Cond, closed *uint32, current, max uint32) ([]*Entry, uint32) {
	idx := atomic.LoadUint32(&w.idx)
	println(idx, current)
	if idx <= current {
		// If we're at the end we'd block forever cause we'll never see another
		// write, so just return the current cursor again. This should never
		// happen because the caller should be checking atEnd manually.
		if w.atEnd(current) {
			return nil, current
		}

		// Block until we have more data. This is the only scenario we need
		// to hold a lock because the buffer will use a condition var to broadcast
		// that data has changed.
		cond.L.Lock()
		for idx <= current {
			cond.Wait()

			// If we closed, exit
			if atomic.LoadUint32(closed) > 0 {
				return nil, current
			}

			// Check the new index
			idx = atomic.LoadUint32(&w.idx)
		}
		cond.L.Unlock()
	}

	end := idx // last index to return, starts with our cursor

	// If the length of items we'd return is more than the maximum
	// we'd want, we just take the maximum.
	if (idx - current) > max {
		end = current + max
	}

	// Return the slice. Note we set the cap() here to be the length
	// returned just so the caller doesn't get any leaked info about
	// our underlying buffer.
	return w.buffer[current:end:end], end
}

// write wites the set of entries into this chunk and returns the number
// of entries written. If the return value is less than the length of
// entries, this means this chunk is full and the remaining entries must
// be written to the next chunk.
func (w *chunk) write(entries []*Entry) int {
	// If we have no buffer then allocate it now. This is safe to do in
	// a concurrent setting because we'll only ever attempt to read
	// w.buffer in read if w.idx > 0.
	if w.buffer == nil {
		w.buffer = make([]*Entry, chunkSize)
	}

	// Write as much of the entries as we can into our buffer starting
	// with our current index.
	n := copy(w.buffer[atomic.LoadUint32(&w.idx):], entries)

	// Move our cursor for the readers
	atomic.AddUint32(&w.idx, uint32(n))

	return n
}
