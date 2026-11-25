package ha

// A standby answers on the API addresses too: it passes each connection to
// the leader unchanged (TLS and all), so a kubeconfig or worker that names
// either control plane reaches the leader (design 3.2).

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// Forward accepts connections on ln until ctx ends and joins each to the
// address target returns at that moment (host:port). A connection that cannot
// reach the leader is closed, which clients see as a refused attempt.
func Forward(ctx context.Context, ln net.Listener, target func() string, logf func(string, ...any)) {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer c.Close()
			addr := target()
			if addr == "" {
				return
			}
			d := net.Dialer{Timeout: 3 * time.Second}
			up, err := d.DialContext(ctx, "tcp", addr)
			if err != nil {
				if logf != nil {
					logf("cannot pass a connection to the leader at %s: %v", addr, err)
				}
				return
			}
			defer up.Close()
			done := make(chan struct{}, 2)
			go func() { io.Copy(up, c); done <- struct{}{} }()
			go func() { io.Copy(c, up); done <- struct{}{} }()
			// TLS and HTTP do not need a half-closed connection: when one
			// direction ends, end both, so a stalled peer cannot hold it.
			select {
			case <-done:
			case <-ctx.Done():
			}
			c.Close()
			up.Close()
			<-done
		}()
	}
}
