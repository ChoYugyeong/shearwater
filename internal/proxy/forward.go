package proxy

import (
	"context"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

type forwarder struct {
	ln      *net.TCPListener
	current atomic.Pointer[route] // immutable snapshot for future connections
	next    atomic.Uint64
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func listen(ctx context.Context, address netip.AddrPort, r route, stillCurrent func(*route) bool, readyNow func(context.Context, *route, backend) bool) (*forwarder, error) {
	ln, err := net.ListenTCP("tcp4", net.TCPAddrFromAddrPort(address))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	// Close AcceptTCP immediately on shutdown even if the manager is waiting
	// for a netlink reply. stop still joins the goroutines before returning.
	context.AfterFunc(ctx, func() { ln.Close() })
	f := &forwarder{ln: ln, cancel: cancel}
	f.current.Store(&r)
	f.wg.Go(func() { f.accept(ctx, stillCurrent, readyNow) })
	return f, nil
}

func (f *forwarder) stop() {
	f.cancel()   // cancel backend dials and close both halves of active streams
	f.ln.Close() // unblock AcceptTCP
	f.wg.Wait()
}

func (f *forwarder) accept(ctx context.Context, stillCurrent func(*route) bool, readyNow func(context.Context, *route, backend) bool) {
	for ctx.Err() == nil {
		client, err := f.ln.AcceptTCP()
		if err != nil {
			// Resource pressure can make accept fail transiently. Avoid a
			// busy loop while leaving the listener in place for recovery.
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-ctx.Done():
			case <-timer.C:
			}
			timer.Stop()
			continue
		}
		f.wg.Go(func() {
			defer client.Close()
			r := f.current.Load()
			if len(r.backends) == 0 || !stillCurrent(r) {
				return
			}
			// Rotate the first candidate across connections. Failed dials
			// fall through to the other candidates within a total time budget.
			start := int((f.next.Add(1) - 1) % uint64(len(r.backends)))
			target := dialBackend(ctx, r.backends, start, func(ctx context.Context, b backend) bool { return readyNow(ctx, r, b) })
			if target == nil {
				return
			}
			defer target.Close()
			if stillCurrent(r) {
				relay(ctx, client, target)
			}
		})
	}
}

func dialBackend(ctx context.Context, backends []backend, start int, readyNow func(context.Context, backend) bool) *net.TCPConn {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	dialer := net.Dialer{Timeout: time.Second}
	for i := 0; i < len(backends) && ctx.Err() == nil; i++ {
		b := backends[(start+i)%len(backends)]
		// Skip stale endpoints before dialing. Check again afterward to
		// cover a Pod exit or reassignment while the dial was in flight.
		if !readyNow(ctx, b) {
			continue
		}
		conn, err := dialer.DialContext(ctx, "tcp4", b.address.String())
		if err == nil {
			if readyNow(ctx, b) {
				return conn.(*net.TCPConn)
			}
			conn.Close()
		}
	}
	return nil
}

// relay carries a byte stream in each direction. A normal EOF closes only the
// destination's write half. Otherwise a protocol that waits for the client's
// EOF before replying would lose its response when the client half-closes.
func relay(ctx context.Context, client, backend *net.TCPConn) {
	closeBoth := func() {
		client.Close()
		backend.Close()
	}
	stop := context.AfterFunc(ctx, closeBoth)
	defer stop()
	copyDirection := func(dst, src *net.TCPConn) {
		if _, err := io.Copy(dst, src); err != nil {
			closeBoth()
		} else if err := dst.CloseWrite(); err != nil {
			closeBoth()
		}
	}
	var wg sync.WaitGroup
	wg.Go(func() { copyDirection(client, backend) })
	copyDirection(backend, client)
	wg.Wait()
}
