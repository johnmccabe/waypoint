package logbuffer

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	pb "github.com/hashicorp/waypoint/internal/server/gen"
)

type TestEntry = pb.LogBatch_Entry

func TestBuffer(t *testing.T) {
	require := require.New(t)

	b := New()
	defer b.Close()

	// Get a reader
	r1 := b.Reader()

	// Write some entries
	b.Write(nil, nil, nil)

	// The reader should be able to get three immediately
	v := r1.Read(10, true)
	require.Len(v, 3)
	require.Equal(3, cap(v))

	// We should block on the next read
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		v = r1.Read(10, true)
	}()

	select {
	case <-doneCh:
		t.Fatal("should block")
	case <-time.After(50 * time.Millisecond):
	}

	// If we request a non-blocking we should get nil
	v = r1.Read(10, false)
	require.Nil(v)

	// Write some more entries which should unblock our reader
	b.Write(nil, nil)
	select {
	case <-doneCh:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("should unblock")
	}

	// Write some more to verify non-blocking reads work
	b.Write(nil, nil, nil, nil)
	v = r1.Read(10, false)
	require.Len(v, 4)
}

func TestBuffer_close(t *testing.T) {
	require := require.New(t)

	b := New()
	defer b.Close()

	// Get a reader
	r1 := b.Reader()

	// We should block on the next read
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		r1.Read(10, true)
	}()

	select {
	case <-doneCh:
		t.Fatal("should block")
	case <-time.After(50 * time.Millisecond):
	}

	// Close our buffer
	require.NoError(b.Close())

	// Should be done
	select {
	case <-doneCh:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("should not block")
	}

	// Should be safe to run
	require.NoError(r1.Close())
}

func TestBuffer_readPartial(t *testing.T) {
	require := require.New(t)

	b := New()
	defer b.Close()

	// Get a reader
	r1 := b.Reader()

	// Write some entries
	b.Write(nil, nil, nil)

	{
		// Get two immediately
		v := r1.Read(2, true)
		require.Len(v, 2)
		require.Equal(2, cap(v))
	}

	{
		// Get the last one
		v := r1.Read(1, true)
		require.Len(v, 1)
		require.Equal(1, cap(v))
	}
}

func TestBuffer_writeFull(t *testing.T) {
	require := require.New(t)

	// Tiny chunks
	chchunk(t, 2, 2)

	// Create a buffer and write a bunch of data. This should overflow easily.
	// We want to verify we don't block or crash.
	b := New()
	defer b.Close()
	for i := 0; i < 53; i++ {
		b.Write(&TestEntry{
			Line: strconv.Itoa(i),
		})
	}

	// Get a reader and get what we can
	r := b.Reader()
	vs := r.Read(10, true)
	require.NotEmpty(vs)
	require.Equal("52", vs[len(vs)-1].(*TestEntry).Line)
}

func TestBuffer_readFull(t *testing.T) {
	require := require.New(t)

	// Tiny chunks
	chchunk(t, 2, 1)

	// Create a buffer and get a reader immediately so we snapshot our
	// current set of buffers.
	b := New()
	defer b.Close()
	r := b.Reader()

	// Write a lot of data to ensure we move the window
	for i := 0; i < 53; i++ {
		b.Write(&TestEntry{
			Line: strconv.Itoa(i),
		})
	}

	// Read the data
	vs := r.Read(1, true)
	require.NotEmpty(vs)
	require.Equal("0", vs[0].(*TestEntry).Line)

	vs = r.Read(1, true)
	require.NotEmpty(vs)
	require.Equal("1", vs[0].(*TestEntry).Line)

	// We jump windows here
	vs = r.Read(1, true)
	require.NotEmpty(vs)
	require.Equal("52", vs[0].(*TestEntry).Line)
}

func TestReader_cancel(t *testing.T) {
	require := require.New(t)

	b := New()
	defer b.Close()

	// Get a reader
	r1 := b.Reader()

	// We should block on the read
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		r1.Read(10, true)
	}()

	select {
	case <-doneCh:
		t.Fatal("should block")
	case <-time.After(50 * time.Millisecond):
	}

	// Close
	require.NoError(r1.Close())

	// Should return
	select {
	case <-doneCh:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("should not block")
	}

	// Should be safe to call again
	require.NoError(r1.Close())
}

func TestReader_cancelContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// Get a reader
	b := New()
	defer b.Close()
	r1 := b.Reader()
	go r1.CloseContext(ctx)

	// We should block on the read
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		r1.Read(10, true)
	}()

	select {
	case <-doneCh:
		t.Fatal("should block")
	case <-time.After(50 * time.Millisecond):
	}

	// Close
	cancel()

	// Should return
	select {
	case <-doneCh:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("should not block")
	}
}

func chchunk(t *testing.T, count, size int) {
	oldcount, oldsize := chunkCount, chunkSize
	t.Cleanup(func() {
		chunkCount = oldcount
		chunkSize = oldsize
	})

	chunkCount, chunkSize = count, size
}
