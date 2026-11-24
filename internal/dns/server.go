package dns

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"shearwater/internal/store"
)

const (
	resyncEvery    = 30 * time.Second
	tcpIdleTimeout = 5 * time.Second
	maxForwardConc = 64
)

// Server is Shearwater's per-node cluster DNS server (UDP + TCP).
// It reads state exclusively through Store (store.Reader) so the same code
// runs on both the control plane and worker nodes.
type Server struct {
	Store store.Reader
	Addr  netip.AddrPort
	// Upstreams overrides host resolver discovery. Nil reads host resolv.conf;
	// a non-nil empty slice disables forwarding (external names get SERVFAIL).
	Upstreams []string
	Logf      func(format string, args ...any)
	// OnListen is called once UDP and TCP listeners are bound (useful in tests).
	OnListen func(udpAddr, tcpAddr netip.AddrPort)

	table atomic.Pointer[Table]
	sem   chan struct{}
}

// SetTable installs a pre-built record table directly (for tests).
func (s *Server) SetTable(t *Table) {
	if t == nil {
		t = NewEmptyTable()
	}
	s.table.Store(t)
}

func (s *Server) currentTable() *Table {
	if t := s.table.Load(); t != nil {
		return t
	}
	return NewEmptyTable()
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, "dns: "+format+"\n", args...)
}

func (s *Server) upstreams() []string {
	if s.Upstreams != nil {
		return s.Upstreams
	}
	return HostUpstreams()
}

// Run rebuilds the record table on store changes, binds UDP and TCP on s.Addr
// (retrying until the gateway address appears), and serves until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	if s.table.Load() == nil {
		s.table.Store(NewEmptyTable())
	}
	s.sem = make(chan struct{}, maxForwardConc)

	var wg sync.WaitGroup
	if s.Store != nil {
		// Prime the table before answering queries when possible.
		if t, err := BuildTable(s.Store); err == nil {
			s.table.Store(t)
		} else {
			s.logf("initial record table build failed: %v", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.watchStore(ctx)
		}()
	}
	defer wg.Wait()

	pc, ln, err := s.bindWithRetry(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	udpAP := pc.LocalAddr().(*net.UDPAddr).AddrPort()
	tcpAP := ln.Addr().(*net.TCPAddr).AddrPort()
	if s.OnListen != nil {
		s.OnListen(udpAP, tcpAP)
	}

	// Close sockets when ctx ends to unblock ReadFrom and Accept.
	go func() {
		<-ctx.Done()
		_ = pc.Close()
		_ = ln.Close()
	}()

	wg.Add(2)
	go func() {
		defer wg.Done()
		s.serveUDP(ctx, pc)
	}()
	go func() {
		defer wg.Done()
		s.serveTCP(ctx, ln)
	}()

	<-ctx.Done()
	return nil
}

// bindWithRetry keeps trying until both UDP and TCP bind on s.Addr. On Linux
// nodes the bridge gateway address is created after startup, so bind may fail
// with EADDRNOTAVAIL briefly.
func (s *Server) bindWithRetry(ctx context.Context) (*net.UDPConn, *net.TCPListener, error) {
	backoff := 200 * time.Millisecond
	logged := false
	for {
		pc, ln, err := s.bindOnce()
		if err == nil {
			return pc, ln, nil
		}
		if !logged {
			s.logf("waiting to bind %s: %v", s.Addr, err)
			logged = true
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, nil, ctx.Err()
		case <-t.C:
		}
		backoff = min(backoff*2, 2*time.Second)
	}
}

func (s *Server) bindOnce() (*net.UDPConn, *net.TCPListener, error) {
	udpAddr := net.UDPAddrFromAddrPort(s.Addr)
	pc, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, nil, err
	}
	// If port 0 was requested (tests), bind TCP to the same chosen port.
	chosen := pc.LocalAddr().(*net.UDPAddr).AddrPort()
	ln, err := net.ListenTCP("tcp", net.TCPAddrFromAddrPort(chosen))
	if err != nil {
		_ = pc.Close()
		return nil, nil, err
	}
	return pc, ln, nil
}

func (s *Server) watchStore(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		changed := s.Store.Changed()
		wait := resyncEvery
		if t, err := BuildTable(s.Store); err != nil {
			s.logf("rebuilding DNS table: %v", err)
			wait, changed = backoff, nil
			backoff = min(backoff*2, 10*time.Second)
		} else {
			s.table.Store(t)
			backoff = time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
		case <-changed:
		case <-timer.C:
		}
		timer.Stop()
	}
}

func (s *Server) serveUDP(ctx context.Context, pc *net.UDPConn) {
	var wg sync.WaitGroup
	defer wg.Wait()
	buf := make([]byte, MaxEDNSUDPSize)
	for {
		n, addr, err := pc.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		pkt := append([]byte(nil), buf[:n]...)
		if resp, ok := s.answerLocal(pkt, false); ok {
			if len(resp) > 0 {
				_, _ = pc.WriteToUDPAddrPort(resp, addr)
			}
			continue
		}
		// Forward external queries asynchronously so a slow upstream never
		// blocks local cluster.local lookups.
		select {
		case s.sem <- struct{}{}:
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-s.sem }()
				if resp := s.answerForward(ctx, pkt, false); len(resp) > 0 {
					_, _ = pc.WriteToUDPAddrPort(resp, addr)
				}
			}()
		default:
			if resp := errorResponse(pkt, RcodeServFail); len(resp) > 0 {
				_, _ = pc.WriteToUDPAddrPort(resp, addr)
			}
		}
	}
}

func (s *Server) serveTCP(ctx context.Context, ln *net.TCPListener) {
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.AcceptTCP()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleTCPConn(ctx, conn)
		}()
	}
}

func (s *Server) handleTCPConn(ctx context.Context, conn *net.TCPConn) {
	defer conn.Close()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	var lenBuf [2]byte
	for {
		_ = conn.SetReadDeadline(time.Now().Add(tcpIdleTimeout))
		if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(lenBuf[:]))
		if n == 0 {
			return
		}
		pkt := make([]byte, n)
		if _, err := io.ReadFull(conn, pkt); err != nil {
			return
		}
		resp, ok := s.answerLocal(pkt, true)
		if !ok {
			resp = s.answerForward(ctx, pkt, true)
		}
		if len(resp) == 0 || len(resp) > 65535 {
			return
		}
		_ = conn.SetWriteDeadline(time.Now().Add(tcpIdleTimeout))
		frame := make([]byte, 2+len(resp))
		binary.BigEndian.PutUint16(frame[0:2], uint16(len(resp)))
		copy(frame[2:], resp)
		if _, err := conn.Write(frame); err != nil {
			return
		}
	}
}

// answerLocal answers cluster queries and malformed/unsupported packets without
// network I/O. Returns handled=false when the packet needs upstream forwarding.
func (s *Server) answerLocal(pkt []byte, viaTCP bool) ([]byte, bool) {
	if len(pkt) < headerLen {
		return nil, true // too short even for a DNS header; drop silently
	}
	req, err := DecodeMessage(pkt)
	if err != nil {
		return errorResponse(pkt, RcodeFormErr), true
	}
	if req.Response {
		return nil, true
	}
	if req.Opcode != OpcodeQuery {
		return replyWithRcode(req, RcodeNotImp, viaTCP), true
	}
	if len(req.Questions) != 1 {
		return replyWithRcode(req, RcodeFormErr, viaTCP), true
	}
	q := req.Questions[0]
	answers, rcode, handled := s.currentTable().Lookup(q)
	if !handled {
		return nil, false
	}
	resp := &Message{
		ID:                 req.ID,
		Response:           true,
		Opcode:             req.Opcode,
		Authoritative:      true,
		RecursionDesired:   req.RecursionDesired,
		RecursionAvailable: true,
		Rcode:              rcode,
		Questions:          req.Questions,
		Answers:            answers,
	}
	maxUDP := 0
	if !viaTCP {
		maxUDP = MaxUDPSize
		if req.EDNS != nil {
			maxUDP = min(max(int(req.EDNS.UDPSize), MaxUDPSize), MaxEDNSUDPSize)
			resp.EDNS = &EDNS0{UDPSize: uint16(maxUDP)}
		}
	} else if req.EDNS != nil {
		resp.EDNS = &EDNS0{UDPSize: MaxEDNSUDPSize}
	}
	wire, err := EncodeMessage(resp, maxUDP)
	if err != nil {
		return replyWithRcode(req, RcodeServFail, viaTCP), true
	}
	return wire, true
}

func (s *Server) answerForward(ctx context.Context, pkt []byte, viaTCP bool) []byte {
	req, err := DecodeMessage(pkt)
	if err != nil {
		return errorResponse(pkt, RcodeFormErr)
	}
	upstreams := s.upstreams()
	respWire, err := Forward(ctx, pkt, req.ID, upstreams, viaTCP)
	if err != nil {
		return replyWithRcode(req, RcodeServFail, viaTCP)
	}
	if !viaTCP {
		maxUDP := MaxUDPSize
		if req.EDNS != nil {
			maxUDP = min(max(int(req.EDNS.UDPSize), MaxUDPSize), MaxEDNSUDPSize)
		}
		if len(respWire) > maxUDP {
			// Re-encode truncated with TC bit set so the client retries on TCP.
			if decoded, derr := DecodeMessage(respWire); derr == nil {
				if trimmed, eerr := EncodeMessage(decoded, maxUDP); eerr == nil {
					return trimmed
				}
			}
			// Fallback: truncate raw header with TC bit set.
			trunc := append([]byte(nil), respWire[:headerLen]...)
			flags := binary.BigEndian.Uint16(trunc[2:4]) | flagTC
			binary.BigEndian.PutUint16(trunc[2:4], flags)
			binary.BigEndian.PutUint16(trunc[6:8], 0)
			binary.BigEndian.PutUint16(trunc[8:10], 0)
			binary.BigEndian.PutUint16(trunc[10:12], 0)
			return trunc
		}
	}
	return respWire
}

func replyWithRcode(req *Message, rcode uint8, viaTCP bool) []byte {
	resp := &Message{
		ID:                 req.ID,
		Response:           true,
		Opcode:             req.Opcode,
		RecursionDesired:   req.RecursionDesired,
		RecursionAvailable: true,
		Rcode:              rcode,
		Questions:          req.Questions,
	}
	maxUDP := 0
	if !viaTCP {
		maxUDP = MaxUDPSize
		if req.EDNS != nil {
			maxUDP = min(max(int(req.EDNS.UDPSize), MaxUDPSize), MaxEDNSUDPSize)
			resp.EDNS = &EDNS0{UDPSize: uint16(maxUDP)}
		}
	}
	wire, _ := EncodeMessage(resp, maxUDP)
	return wire
}

func errorResponse(pkt []byte, rcode uint8) []byte {
	if len(pkt) < headerLen {
		return nil
	}
	out := make([]byte, headerLen)
	copy(out[0:2], pkt[0:2])
	inFlags := binary.BigEndian.Uint16(pkt[2:4])
	flags := flagQR | (inFlags & (0x0f << 11)) | (inFlags & flagRD) | flagRA | uint16(rcode&0x0f)
	binary.BigEndian.PutUint16(out[2:4], flags)
	return out
}
