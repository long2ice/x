package net

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/go-gost/core/common/bufpool"
	xio "github.com/go-gost/x/internal/io"
)

const (
	// tcpWaitTimeout implements a TCP half-close timeout.
	tcpWaitTimeout = 10 * time.Second
)

func Pipe(ctx context.Context, rw1, rw2 io.ReadWriteCloser) error {
	return pipe(ctx, rw1, rw2, 0)
}

// PipeIdle behaves like Pipe but bounds each direction by an idle timeout: if
// no bytes are read from src for idleTimeout the goroutine returns and
// initiates the half-close. Set idleTimeout to 0 to disable (equivalent to
// Pipe). Useful for proxied TCP that may be silently abandoned by a peer
// (e.g. mobile network switch) so the server doesn't have to wait for TCP
// keepalive (~5 min) to reclaim the goroutine and gVisor TCB.
func PipeIdle(ctx context.Context, rw1, rw2 io.ReadWriteCloser, idleTimeout time.Duration) error {
	return pipe(ctx, rw1, rw2, idleTimeout)
}

func pipe(ctx context.Context, rw1, rw2 io.ReadWriteCloser, idle time.Duration) error {
	wg := sync.WaitGroup{}
	wg.Add(2)

	ch := make(chan error, 2)

	go func() {
		defer wg.Done()
		if err := pipeBuffer(rw1, rw2, bufferSize/2, idle); err != nil {
			ch <- err
		}
	}()
	go func() {
		defer wg.Done()
		if err := pipeBuffer(rw2, rw1, bufferSize/2, idle); err != nil {
			ch <- err
		}
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		// Force-close so the pipe goroutines exit promptly instead of
		// waiting up to idleTimeout / TCP keepalive for the next Read.
		rw1.Close()
		rw2.Close()
		return nil
	}

	select {
	case err := <-ch:
		return err
	default:
	}

	return nil
}

type readDeadlineSetter interface {
	SetReadDeadline(time.Time) error
}

func pipeBuffer(dst io.ReadWriteCloser, src io.ReadWriteCloser, bufferSize int, idle time.Duration) error {
	buf := bufpool.Get(bufferSize)
	defer bufpool.Put(buf)

	var err error
	if idle > 0 {
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
