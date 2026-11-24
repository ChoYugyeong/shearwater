package dns

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	systemdResolvPath = "/run/systemd/resolve/resolv.conf"
	etcResolvPath     = "/etc/resolv.conf"
	upstreamTimeout   = 2 * time.Second
)

var (
	loopbackV4     = netip.MustParsePrefix("127.0.0.0/8")
	errNoUpstreams = errors.New("dns: no upstream nameservers configured")
)

// HostUpstreams reads upstream resolvers from /run/systemd/resolve/resolv.conf
// when present, otherwise /etc/resolv.conf, skipping 127.0.0.0/8 and ::1.
func HostUpstreams() []string {
	for _, path := range []string{systemdResolvPath, etcResolvPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if list := ParseResolvConf(data); len(list) > 0 {
			return list
		}
	}
	return nil
}

// ParseResolvConf extracts non-loopback nameserver host:53 addresses from a
// resolv.conf file.
func ParseResolvConf(data []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if i := strings.IndexAny(line, "#;"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		ip, err := netip.ParseAddr(fields[1])
		if err != nil {
			continue
		}
		ip = ip.Unmap()
		if loopbackV4.Contains(ip) || ip.IsLoopback() {
			continue
		}
		hp := net.JoinHostPort(ip.String(), "53")
		if !slices.Contains(out, hp) {
			out = append(out, hp)
		}
	}
	return out
}

// Forward sends a raw DNS query to upstreams in order until one replies.
func Forward(ctx context.Context, rawQuery []byte, queryID uint16, upstreams []string, viaTCP bool) ([]byte, error) {
	if len(upstreams) == 0 {
		return nil, errNoUpstreams
	}
	var lastErr error
	for _, addr := range upstreams {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var resp []byte
		var err error
		if viaTCP {
			resp, err = exchangeTCP(ctx, addr, rawQuery, queryID)
		} else {
			resp, err = exchangeUDP(ctx, addr, rawQuery, queryID)
		}
		if err == nil {
			return resp, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func exchangeUDP(ctx context.Context, addr string, query []byte, queryID uint16) ([]byte, error) {
	d := net.Dialer{Timeout: upstreamTimeout}
	conn, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	deadline := time.Now().Add(upstreamTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, MaxEDNSUDPSize)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return nil, err
		}
		if n < headerLen {
			continue
		}
		if binary.BigEndian.Uint16(buf[0:2]) != queryID {
			continue
		}
		return append([]byte(nil), buf[:n]...), nil
	}
}

func exchangeTCP(ctx context.Context, addr string, query []byte, queryID uint16) ([]byte, error) {
	if len(query) > 65535 {
		return nil, ErrMessageHuge
	}
	d := net.Dialer{Timeout: upstreamTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	deadline := time.Now().Add(upstreamTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	frame := make([]byte, 2+len(query))
	binary.BigEndian.PutUint16(frame[0:2], uint16(len(query)))
	copy(frame[2:], query)
	if _, err := conn.Write(frame); err != nil {
		return nil, err
	}
	var lenBuf [2]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(lenBuf[:]))
	if n < headerLen {
		return nil, ErrTruncated
	}
	resp := make([]byte, n)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return nil, err
	}
	if binary.BigEndian.Uint16(resp[0:2]) != queryID {
		return nil, fmt.Errorf("dns: upstream TCP ID mismatch")
	}
	return resp, nil
}
