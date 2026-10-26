package net

import (
	"context"
	"io"
	"sync/atomic"
	"time"

	"github.com/go-gost/core/common/bufpool"
	xio "github.com/go-gost/x/internal/io"
)

const (
	// tcpWaitTimeout implements a TCP half-close timeout.
	tcpWaitTimeout = 10 * time.Second
)

func Pipe(ctx context.Context, rw1, rw2 io.ReadWriteCloser) error {
	return pipe(ctx, rw1, rw2, 0, bufferSize/2, false)
}

// PipeIdle bounds inactivity across the whole connection. Traffic in either
// direction keeps both copy loops alive. Zero disables idle reclamation.
func PipeIdle(ctx context.Context, rw1, rw2 io.ReadWriteCloser, idleTimeout time.Duration) error {
	return PipeIdleBuffer(ctx, rw1, rw2, idleTimeout, bufferSize/2)
}

// PipeIdleBuffer behaves like PipeIdle but uses bufferSize bytes for each copy
// direction and applies idleTimeout to the connection as a whole: activity in
// either direction keeps both directions alive. A non-positive size keeps the
// regular PipeIdle buffer default.
func PipeIdleBuffer(ctx context.Context, rw1, rw2 io.ReadWriteCloser, idleTimeout time.Duration, bufferSize int) error {
	if bufferSize <= 0 {
		bufferSize = defaultPipeBufferSize()
	}
	return pipe(ctx, rw1, rw2, idleTimeout, bufferSize, true)
}

func defaultPipeBufferSize() int {
	return bufferSize / 2
}

func pipe(ctx context.Context, rw1, rw2 io.ReadWriteCloser, idle time.Duration, copyBufferSize int, sharedIdle bool) error {
	// The channel is buffered so both copy goroutines can always deliver
	// their result and exit, even when this goroutine has already returned
	// on ctx cancellation. The calling goroutine collects the results
	// itself: with one pipe per proxied connection, a dedicated watcher
	// goroutine just to wait on the pair is real memory and scheduler load
	// on a busy node.
	ch := make(chan error, 2)

	copyIdle := idle
	var activity *pipeActivity
	var idleTimer *time.Timer
	var idleTimerC <-chan time.Time
	if sharedIdle {
		copyIdle = 0
		// Keep using the supplied buffer even when idle reclamation is disabled.
		// io.CopyBuffer's TCP fast paths can bypass it and allocate another one.
		activity = newPipeActivity()
		if idle > 0 {
			idleTimer = time.NewTimer(idle)
			idleTimerC = idleTimer.C
			defer idleTimer.Stop()
		}
	}

	go func() {
		ch <- pipeBuffer(rw1, rw2, copyBufferSize, copyIdle, activity)
	}()
	go func() {
		ch <- pipeBuffer(rw2, rw1, copyBufferSize, copyIdle, activity)
	}()

	var err error
	for remaining := 2; remaining > 0; {
		select {
		case e := <-ch:
			remaining--
			if err == nil {
				err = e
			}
		case <-idleTimerC:
			if wait := activity.remaining(idle); wait > 0 {
				idleTimer.Reset(wait)
				continue
			}
			// Neither direction has moved data for the complete idle window.
			// Close both sides to unblock their copy goroutines immediately.
			rw1.Close()
			rw2.Close()
			return nil
		case <-ctx.Done():
			// Force-close so the pipe goroutines exit promptly instead of
			// waiting up to idleTimeout / TCP keepalive for the next Read.
			rw1.Close()
			rw2.Close()
			return nil
		}
	}
	return err
}

type readDeadlineSetter interface {
	SetReadDeadline(time.Time) error
}

type pipeActivity struct {
	start time.Time
	last  atomic.Int64
}

func newPipeActivity() *pipeActivity {
	return &pipeActivity{start: time.Now()}
}

func (a *pipeActivity) touch() {
	stamp := time.Since(a.start).Nanoseconds()
	for previous := a.last.Load(); stamp > previous; previous = a.last.Load() {
		if a.last.CompareAndSwap(previous, stamp) {
			return
		}
	}
}

func (a *pipeActivity) remaining(idle time.Duration) time.Duration {
	idleFor := time.Since(a.start) - time.Duration(a.last.Load())
	if idleFor >= idle {
		return 0
	}
	return idle - idleFor
}

type activityReadWriter struct {
	io.ReadWriter
	activity *pipeActivity
}

func (rw *activityReadWriter) Read(p []byte) (int, error) {
	n, err := rw.ReadWriter.Read(p)
	if n > 0 {
		rw.activity.touch()
	}
	return n, err
}

// PipeIdleFunc runs a protocol-aware copier in each direction. It tracks reads
// within frames, closes both endpoints on exit, and waits for both workers.
func PipeIdleFunc(ctx context.Context, client, target io.ReadWriteCloser, idle time.Duration,
	copyFn func(context.Context, io.ReadWriter, io.ReadWriter, bool) error) error {
	copyCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	activity := newPipeActivity()
	left := &activityReadWriter{ReadWriter: client, activity: activity}
	right := &activityReadWriter{ReadWriter: target, activity: activity}
	done := make(chan error, 2)
	go func() { done <- copyFn(copyCtx, right, left, true) }()
	go func() { done <- copyFn(copyCtx, left, right, false) }()
	var timer *time.Timer
	var expired <-chan time.Time
	if idle > 0 {
		timer = time.NewTimer(idle)
		expired = timer.C
		defer timer.Stop()
	}
	var err error
	remaining := 2
wait:
	for {
		select {
		case err = <-done:
			remaining--
			break wait
		case <-ctx.Done():
			break wait
		case <-expired:
			if delay := activity.remaining(idle); delay > 0 {
				timer.Reset(delay)
				continue
			}
			break wait
		}
	}
	cancel()
	client.Close()
	target.Close()
	for ; remaining > 0; remaining-- {
		<-done
	}
	return err
}

func pipeBuffer(dst io.ReadWriteCloser, src io.ReadWriteCloser, bufferSize int, idle time.Duration, activity *pipeActivity) error {
	buf := bufpool.Get(bufferSize)
	defer bufpool.Put(buf)

	var err error
	if activity != nil {
		err = copyBufferActivity(dst, src, buf, activity)
	} else if idle > 0 {
		err = copyBufferIdle(dst, src, buf, idle)
	} else {
		_, err = io.CopyBuffer(dst, src, buf)
	}

	// Do the upload/download side TCP half-close.
	if cr, ok := src.(xio.CloseRead); ok {
		cr.CloseRead()
	}

	if cw, ok := dst.(xio.CloseWrite); ok {
		if e := cw.CloseWrite(); e == xio.ErrUnsupported {
			dst.Close()
		} else {
			// Set TCP half-close timeout.
			xio.SetReadDeadline(dst, time.Now().Add(tcpWaitTimeout))
		}
	} else {
		dst.Close()
	}

	return err
}

func copyBufferActivity(dst, src io.ReadWriteCloser, buf []byte, activity *pipeActivity) error {
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			activity.touch()
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return nil
			}
			return rerr
		}
	}
}

// copyBufferIdle is io.CopyBuffer with a per-Read idle deadline. The deadline
// is reset before every Read so it bounds *consecutive* idle time only;
// active connections are unaffected.
func copyBufferIdle(dst, src io.ReadWriteCloser, buf []byte, idle time.Duration) error {
	rds, _ := src.(readDeadlineSetter)
	for {
		if rds != nil {
			_ = rds.SetReadDeadline(time.Now().Add(idle))
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return nil
			}
			return rerr
		}
	}
}
