package dns

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

func TestWireRoundTripAndCompression(t *testing.T) {
	msg := &Message{
		ID:                 0x1234,
		Response:           true,
		Authoritative:      true,
		RecursionDesired:   true,
		RecursionAvailable: true,
		Rcode:              RcodeNoError,
		Questions: []Question{
			{Name: "web.default.svc.cluster.local.", Type: TypeA, Class: ClassIN},
		},
		Answers: []Resource{
			{Name: "web.default.svc.cluster.local.", Type: TypeA, Class: ClassIN, TTL: 5, A: netip.MustParseAddr("10.96.0.10")},
			{Name: "web.default.svc.cluster.local.", Type: TypeA, Class: ClassIN, TTL: 5, A: netip.MustParseAddr("10.96.0.11")},
			{Name: "_http._tcp.web.default.svc.cluster.local.", Type: TypeSRV, Class: ClassIN, TTL: 5,
				SRV: &SRVRecord{Priority: 0, Weight: 100, Port: 80, Target: "web.default.svc.cluster.local."}},
			{Name: "10.0.96.10.in-addr.arpa.", Type: TypePTR, Class: ClassIN, TTL: 5, PTR: "web.default.svc.cluster.local."},
		},
		EDNS: &EDNS0{UDPSize: 1232, DO: true},
	}

	wire, err := EncodeMessage(msg, MaxEDNSUDPSize)
	if err != nil {
		t.Fatalf("EncodeMessage: %v", err)
	}
	// Verify SRV target was NOT compressed (RFC 2782) by checking that the full
	// uncompressed label sequence for "web.default.svc.cluster.local." appears
	// at least twice (once in Question, once in SRV Target).
	uncompressedTarget := []byte{3, 'w', 'e', 'b', 7, 'd', 'e', 'f', 'a', 'u', 'l', 't', 3, 's', 'v', 'c', 7, 'c', 'l', 'u', 's', 't', 'e', 'r', 5, 'l', 'o', 'c', 'a', 'l', 0}
	if count := bytes.Count(wire, uncompressedTarget); count < 2 {
		t.Fatalf("expected uncompressed name in both question and SRV target, found %d occurrences", count)
	}
	// Compression must keep the packet noticeably smaller than fully uncompressed.
	if len(wire) >= 220 {
		t.Fatalf("expected name compression to keep packet compact, got %d bytes", len(wire))
	}

	got, err := DecodeMessage(wire)
	if err != nil {
		t.Fatalf("DecodeMessage: %v", err)
	}
	if got.ID != msg.ID || !got.Response || !got.Authoritative || got.Rcode != RcodeNoError {
		t.Fatalf("header mismatch: %+v", got)
	}
	if len(got.Questions) != 1 || got.Questions[0] != msg.Questions[0] {
		t.Fatalf("questions mismatch: %+v", got.Questions)
	}
	if len(got.Answers) != 4 {
		t.Fatalf("got %d answers, want 4", len(got.Answers))
	}
	if got.Answers[0].A != netip.MustParseAddr("10.96.0.10") || got.Answers[1].A != netip.MustParseAddr("10.96.0.11") {
		t.Fatalf("A records mismatch: %+v", got.Answers[:2])
	}
	if srv := got.Answers[2].SRV; srv == nil || srv.Port != 80 || srv.Target != "web.default.svc.cluster.local." {
		t.Fatalf("SRV mismatch: %+v", got.Answers[2])
	}
	if got.Answers[3].PTR != "web.default.svc.cluster.local." {
		t.Fatalf("PTR mismatch: %+v", got.Answers[3])
	}
	if got.EDNS == nil || got.EDNS.UDPSize != 1232 || !got.EDNS.DO {
		t.Fatalf("EDNS mismatch: %+v", got.EDNS)
	}
}

func TestWireMalformedPacketsNeverPanic(t *testing.T) {
	// Valid base packet for truncation and byte-flip fuzzing.
	sample, err := EncodeMessage(&Message{
		ID:        0xabcd,
		Response:  true,
		Questions: []Question{{Name: "api.prod.svc.cluster.local.", Type: TypeA, Class: ClassIN}},
		Answers: []Resource{
			{Name: "api.prod.svc.cluster.local.", Type: TypeA, Class: ClassIN, TTL: 5, A: netip.MustParseAddr("10.96.0.5")},
			{Name: "5.0.96.10.in-addr.arpa.", Type: TypePTR, Class: ClassIN, TTL: 5, PTR: "api.prod.svc.cluster.local."},
		},
		EDNS: &EDNS0{UDPSize: 1232},
	}, MaxEDNSUDPSize)
	if err != nil {
		t.Fatal(err)
	}
	// Every truncation must return an error cleanly.
	for i := 0; i < len(sample); i++ {
		if _, err := DecodeMessage(sample[:i]); err == nil {
			t.Errorf("truncation at %d/%d unexpectedly succeeded", i, len(sample))
		}
	}
	// Every single-byte mutation must never panic.
	for i := 0; i < len(sample); i++ {
		for _, mask := range []byte{0xff, 0xc0, 0x80, 0x40, 0x01} {
			mut := bytes.Clone(sample)
			mut[i] ^= mask
			_, _ = DecodeMessage(mut)
		}
	}

	// Explicit adversarial packets.
	cases := []struct {
		name string
		pkt  []byte
		want error
	}{
		{"empty", nil, ErrTruncated},
		{"short header", []byte{1, 2, 3}, ErrTruncated},
		{"self compression loop", []byte{0, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0xc0, 0x0c, 0, 1, 0, 1}, ErrNameLoop},
		{"two-pointer cycle", []byte{0, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0xc0, 0x0e, 0xc0, 0x0c, 0, 1, 0, 1}, ErrNameLoop},
		{"pointer out of bounds", []byte{0, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0xc0, 0x99, 0, 1, 0, 1}, ErrBadName},
		{"reserved label type 0x40", []byte{0, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0x41, 'a', 0, 0, 1, 0, 1}, ErrBadName},
		{"reserved label type 0x80", []byte{0, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0x81, 'a', 0, 0, 1, 0, 1}, ErrBadName},
		{"absurd qdcount", []byte{0, 1, 0, 0, 0xff, 0xff, 0, 0, 0, 0, 0, 0}, ErrTruncated},
		{"absurd ancount", []byte{0, 1, 0, 0, 0, 0, 0xff, 0xff, 0, 0, 0, 0}, ErrTruncated},
		{"truncated label", []byte{0, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 10, 'a', 'b'}, ErrTruncated},
	}
	for _, tc := range cases {
		_, err := DecodeMessage(tc.pkt)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: got err=%v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestUDPTruncationSetsTCAndTCPCarriesAll(t *testing.T) {
	// Build a headless Service with 40 ready pod IPs so the response exceeds 512 bytes.
	var eps []apis.Endpoint
	for i := 2; i < 42; i++ {
		eps = append(eps, apis.Endpoint{
			Addresses:  []string{fmt.Sprintf("10.244.0.%d", i)},
			Conditions: apis.EndpointConditions{Ready: new(true)},
		})
	}
	tbl := BuildTableFrom(
		[]apis.Service{{
			Metadata: apis.ObjectMeta{Namespace: "default", Name: "many"},
			Spec:     apis.ServiceSpec{Type: "ClusterIP", ClusterIP: "None", Ports: []apis.ServicePort{{Name: "http", Port: 80, Protocol: "TCP"}}},
		}},
		[]apis.EndpointSlice{{
			Metadata:    apis.ObjectMeta{Namespace: "default", Name: "many-1", Labels: map[string]string{apis.ServiceNameLabel: "many"}},
			AddressType: "IPv4",
			Endpoints:   eps,
		}},
		nil,
	)

	srv, addr := startTestServer(t, nil, tbl, []string{})

	// Plain UDP (512 byte limit): must set TC=1 and stay <= 512 bytes.
	q := &Message{ID: 7, RecursionDesired: true, Questions: []Question{{Name: "many.default.svc.cluster.local.", Type: TypeA, Class: ClassIN}}}
	rawQ, err := EncodeMessage(q, MaxUDPSize)
	if err != nil {
		t.Fatal(err)
	}
	rawResp, err := exchangeUDP(context.Background(), addr.String(), rawQ, q.ID)
	if err != nil {
		t.Fatalf("UDP query: %v", err)
	}
	if len(rawResp) > MaxUDPSize {
		t.Fatalf("UDP response %d bytes exceeds %d", len(rawResp), MaxUDPSize)
	}
	resp, err := DecodeMessage(rawResp)
	if err != nil {
		t.Fatalf("decode UDP response: %v", err)
	}
	if !resp.Truncated || len(resp.Answers) >= 40 {
		t.Fatalf("expected TC=1 and truncated answer list, got TC=%v answers=%d", resp.Truncated, len(resp.Answers))
	}

	// UDP with EDNS0 (4096 bytes): must fit all 40 A records with TC=0.
	qEDNS := &Message{ID: 8, RecursionDesired: true, Questions: q.Questions, EDNS: &EDNS0{UDPSize: 4096}}
	rawQEDNS, err := EncodeMessage(qEDNS, MaxEDNSUDPSize)
	if err != nil {
		t.Fatal(err)
	}
	rawRespEDNS, err := exchangeUDP(context.Background(), addr.String(), rawQEDNS, qEDNS.ID)
	if err != nil {
		t.Fatalf("EDNS UDP query: %v", err)
	}
	respEDNS, err := DecodeMessage(rawRespEDNS)
	if err != nil {
		t.Fatalf("decode EDNS response: %v", err)
	}
	if respEDNS.Truncated || len(respEDNS.Answers) != 40 {
		t.Fatalf("expected 40 answers without truncation over EDNS0, got TC=%v answers=%d", respEDNS.Truncated, len(respEDNS.Answers))
	}

	// TCP (2-byte length framing): must fit all 40 A records with TC=0.
	rawRespTCP, err := exchangeTCP(context.Background(), addr.String(), rawQ, q.ID)
	if err != nil {
		t.Fatalf("TCP query: %v", err)
	}
	respTCP, err := DecodeMessage(rawRespTCP)
	if err != nil {
		t.Fatalf("decode TCP response: %v", err)
	}
	if respTCP.Truncated || len(respTCP.Answers) != 40 {
		t.Fatalf("expected 40 answers without truncation over TCP, got TC=%v answers=%d", respTCP.Truncated, len(respTCP.Answers))
	}
	_ = srv
}

func TestRecordTableClusterIPHeadlessSRVPodPTR(t *testing.T) {
	p8080 := int32(8080)
	pName := "http"
	pProto := "TCP"
	tbl := BuildTableFrom(
		[]apis.Service{
			{
				Metadata: apis.ObjectMeta{Namespace: "shop", Name: "api"},
				Spec: apis.ServiceSpec{
					Type:      "ClusterIP",
					ClusterIP: "10.96.0.20",
					Ports:     []apis.ServicePort{{Name: "http", Port: 80, Protocol: "TCP"}, {Name: "metrics", Port: 9090, Protocol: "TCP"}},
				},
			},
			{
				Metadata: apis.ObjectMeta{Namespace: "shop", Name: "db"},
				Spec: apis.ServiceSpec{
					Type:      "ClusterIP",
					ClusterIP: "None",
					Ports:     []apis.ServicePort{{Name: "http", Port: 80, Protocol: "TCP"}},
				},
			},
		},
		[]apis.EndpointSlice{{
			Metadata:    apis.ObjectMeta{Namespace: "shop", Name: "db-abc", Labels: map[string]string{apis.ServiceNameLabel: "db"}},
			AddressType: "IPv4",
			Ports:       []apis.EndpointPort{{Name: &pName, Protocol: &pProto, Port: &p8080}},
			Endpoints: []apis.Endpoint{
				{Addresses: []string{"10.244.0.12"}, Conditions: apis.EndpointConditions{Ready: new(true)}},
				{Addresses: []string{"10.244.0.10"}, Conditions: apis.EndpointConditions{Ready: new(true)}},
				{Addresses: []string{"10.244.0.11"}, Conditions: apis.EndpointConditions{Ready: new(false)}},
				{Addresses: []string{"10.244.0.13"}, Conditions: apis.EndpointConditions{Ready: new(true), Terminating: new(true)}},
			},
		}},
		[]apis.Pod{
			{Metadata: apis.ObjectMeta{Namespace: "shop", Name: "p1"}, Status: apis.PodStatus{Phase: "Running", PodIP: "10.244.0.10"}},
			{Metadata: apis.ObjectMeta{Namespace: "shop", Name: "done"}, Status: apis.PodStatus{Phase: "Succeeded", PodIP: "10.244.0.99"}},
		},
	)

	// ClusterIP Service A + case-insensitivity.
	ans, rcode, handled := tbl.Lookup(Question{Name: "API.shop.svc.cluster.local.", Type: TypeA, Class: ClassIN})
	if !handled || rcode != RcodeNoError || len(ans) != 1 || ans[0].A != netip.MustParseAddr("10.96.0.20") || ans[0].TTL != DefaultTTL {
		t.Fatalf("ClusterIP lookup = %+v rcode=%d handled=%v", ans, rcode, handled)
	}

	// Headless Service A: only ready, non-terminating pod IPs, sorted.
	ans, rcode, handled = tbl.Lookup(Question{Name: "db.shop.svc.cluster.local.", Type: TypeA, Class: ClassIN})
	if !handled || rcode != RcodeNoError || len(ans) != 2 ||
		ans[0].A != netip.MustParseAddr("10.244.0.10") || ans[1].A != netip.MustParseAddr("10.244.0.12") {
		t.Fatalf("headless lookup = %+v rcode=%d handled=%v", ans, rcode, handled)
	}

	// ClusterIP SRV for named port.
	ans, rcode, handled = tbl.Lookup(Question{Name: "_metrics._tcp.api.shop.svc.cluster.local.", Type: TypeSRV, Class: ClassIN})
	if !handled || rcode != RcodeNoError || len(ans) != 1 || ans[0].SRV.Port != 9090 || ans[0].SRV.Target != "api.shop.svc.cluster.local." {
		t.Fatalf("ClusterIP SRV = %+v rcode=%d handled=%v", ans, rcode, handled)
	}

	// Headless SRV for named port points to ready pod FQDNs with endpoint target port.
	ans, rcode, handled = tbl.Lookup(Question{Name: "_http._tcp.db.shop.svc.cluster.local.", Type: TypeSRV, Class: ClassIN})
	if !handled || rcode != RcodeNoError || len(ans) != 2 || ans[0].SRV.Port != 8080 || ans[0].SRV.Target != "10-244-0-10.shop.pod.cluster.local." {
		t.Fatalf("headless SRV = %+v rcode=%d handled=%v", ans, rcode, handled)
	}

	// Pod A record (running present, Succeeded absent).
	ans, rcode, handled = tbl.Lookup(Question{Name: "10-244-0-10.shop.pod.cluster.local.", Type: TypeA, Class: ClassIN})
	if !handled || rcode != RcodeNoError || len(ans) != 1 || ans[0].A != netip.MustParseAddr("10.244.0.10") {
		t.Fatalf("pod A = %+v rcode=%d handled=%v", ans, rcode, handled)
	}
	_, rcode, handled = tbl.Lookup(Question{Name: "10-244-0-99.shop.pod.cluster.local.", Type: TypeA, Class: ClassIN})
	if !handled || rcode != RcodeNXDomain {
		t.Fatalf("succeeded pod must be NXDOMAIN, got rcode=%d handled=%v", rcode, handled)
	}

	// PTR for Service IP and Pod IP.
	ans, rcode, handled = tbl.Lookup(Question{Name: "20.0.96.10.in-addr.arpa.", Type: TypePTR, Class: ClassIN})
	if !handled || rcode != RcodeNoError || len(ans) != 1 || ans[0].PTR != "api.shop.svc.cluster.local." {
		t.Fatalf("service PTR = %+v rcode=%d handled=%v", ans, rcode, handled)
	}
	ans, rcode, handled = tbl.Lookup(Question{Name: "10.0.244.10.in-addr.arpa.", Type: TypePTR, Class: ClassIN})
	if !handled || rcode != RcodeNoError || len(ans) != 1 || ans[0].PTR != "10-244-0-10.shop.pod.cluster.local." {
		t.Fatalf("pod PTR = %+v rcode=%d handled=%v", ans, rcode, handled)
	}
	// Unknown cluster IP PTR -> NXDOMAIN (not forwarded).
	_, rcode, handled = tbl.Lookup(Question{Name: "55.0.96.10.in-addr.arpa.", Type: TypePTR, Class: ClassIN})
	if !handled || rcode != RcodeNXDomain {
		t.Fatalf("unknown cluster IP PTR = rcode=%d handled=%v, want NXDOMAIN", rcode, handled)
	}
	// External IP PTR -> not handled (forward upstream).
	_, _, handled = tbl.Lookup(Question{Name: "8.8.8.8.in-addr.arpa.", Type: TypePTR, Class: ClassIN})
	if handled {
		t.Fatal("external PTR must be forwarded upstream")
	}

	// Known cluster name AAAA -> NOERROR empty; unknown cluster name -> NXDOMAIN.
	ans, rcode, handled = tbl.Lookup(Question{Name: "api.shop.svc.cluster.local.", Type: TypeAAAA, Class: ClassIN})
	if !handled || rcode != RcodeNoError || len(ans) != 0 {
		t.Fatalf("AAAA on known name = %+v rcode=%d handled=%v, want empty NOERROR", ans, rcode, handled)
	}
	_, rcode, handled = tbl.Lookup(Question{Name: "missing.shop.svc.cluster.local.", Type: TypeA, Class: ClassIN})
	if !handled || rcode != RcodeNXDomain {
		t.Fatalf("unknown cluster name = rcode=%d handled=%v, want NXDOMAIN", rcode, handled)
	}
	_, _, handled = tbl.Lookup(Question{Name: "example.com.", Type: TypeA, Class: ClassIN})
	if handled {
		t.Fatal("external domain must not be handled locally")
	}
}

func TestServerLiveStoreUpdatesAndUpstreamForwarding(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Start a fake upstream DNS server on 127.0.0.1:0 (UDP + TCP).
	upstreamAddr := startFakeUpstream(t, map[string]netip.Addr{
		"example.com.": netip.MustParseAddr("93.184.216.34"),
	})

	_, addr := startTestServer(t, st, nil, []string{upstreamAddr.String()})

	query := func(viaTCP bool, name string, qtype uint16) *Message {
		t.Helper()
		q := &Message{ID: 42, RecursionDesired: true, Questions: []Question{{Name: name, Type: qtype, Class: ClassIN}}}
		rawQ, err := EncodeMessage(q, MaxUDPSize)
		if err != nil {
			t.Fatal(err)
		}
		var rawResp []byte
		if viaTCP {
			rawResp, err = exchangeTCP(context.Background(), addr.String(), rawQ, q.ID)
		} else {
			rawResp, err = exchangeUDP(context.Background(), addr.String(), rawQ, q.ID)
		}
		if err != nil {
			t.Fatalf("query %s (tcp=%v): %v", name, viaTCP, err)
		}
		resp, err := DecodeMessage(rawResp)
		if err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return resp
	}

	// Before Service exists: NXDOMAIN.
	if resp := query(false, "web.default.svc.cluster.local.", TypeA); resp.Rcode != RcodeNXDomain {
		t.Fatalf("want NXDOMAIN before Service exists, got rcode=%d", resp.Rcode)
	}

	// Create Service in store; DNS server must rebuild its table via store.Reader.Changed().
	res := apis.Lookup("", "v1", "services")
	svc := apis.Service{
		Metadata: apis.ObjectMeta{Namespace: "default", Name: "web"},
		Spec:     apis.ServiceSpec{Type: "ClusterIP", ClusterIP: "10.96.0.42", Ports: []apis.ServicePort{{Name: "http", Port: 80, Protocol: "TCP"}}},
	}
	objects.Stamp(res, &svc)
	if err := st.Write(false, func(tx *store.Tx) error { _, e := tx.Create(objects.Put(res, &svc)); return e }); err != nil {
		t.Fatal(err)
	}

	var gotIP netip.Addr
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(15 * time.Millisecond) {
		resp := query(false, "web.default.svc.cluster.local.", TypeA)
		if resp.Rcode == RcodeNoError && len(resp.Answers) == 1 {
			gotIP = resp.Answers[0].A
			break
		}
	}
	if gotIP != netip.MustParseAddr("10.96.0.42") {
		t.Fatalf("live store update did not appear in DNS, got %v", gotIP)
	}

	// Upstream forwarding over both UDP and TCP.
	for _, viaTCP := range []bool{false, true} {
		resp := query(viaTCP, "example.com.", TypeA)
		if resp.Rcode != RcodeNoError || len(resp.Answers) != 1 || resp.Answers[0].A != netip.MustParseAddr("93.184.216.34") {
			t.Fatalf("upstream forwarding (tcp=%v) failed: %+v", viaTCP, resp)
		}
	}

	// Without upstreams, external names must return SERVFAIL.
	_, noUpAddr := startTestServer(t, nil, NewEmptyTable(), []string{})
	q := &Message{ID: 99, RecursionDesired: true, Questions: []Question{{Name: "example.com.", Type: TypeA, Class: ClassIN}}}
	rawQ, _ := EncodeMessage(q, MaxUDPSize)
	rawResp, err := exchangeUDP(context.Background(), noUpAddr.String(), rawQ, q.ID)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := DecodeMessage(rawResp)
	if err != nil || resp.Rcode != RcodeServFail {
		t.Fatalf("want SERVFAIL without upstreams, got rcode=%d err=%v", resp.Rcode, err)
	}
}

func TestParseResolvConfFiltersLoopback(t *testing.T) {
	conf := []byte(`
# comment
nameserver 127.0.0.53
nameserver 127.0.0.1
nameserver ::1
nameserver 1.1.1.1 # cloudflare
nameserver 8.8.8.8
nameserver 1.1.1.1
nameserver not-an-ip
nameserver 2606:4700:4700::1111
`)
	got := ParseResolvConf(conf)
	want := []string{"1.1.1.1:53", "8.8.8.8:53", "[2606:4700:4700::1111]:53"}
	if !slices.Equal(got, want) {
		t.Fatalf("ParseResolvConf = %v, want %v", got, want)
	}
}

func startTestServer(t *testing.T, r store.Reader, tbl *Table, upstreams []string) (*Server, netip.AddrPort) {
	t.Helper()
	ready := make(chan netip.AddrPort, 1)
	srv := &Server{
		Store:     r,
		Addr:      netip.MustParseAddrPort("127.0.0.1:0"),
		Upstreams: upstreams,
		Logf:      func(string, ...any) {},
		OnListen:  func(udpAddr, _ netip.AddrPort) { ready <- udpAddr },
	}
	if tbl != nil {
		srv.SetTable(tbl)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("dns server exited with error: %v", err)
		}
	})
	select {
	case addr := <-ready:
		return srv, addr
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for test DNS server to listen")
		return nil, netip.AddrPort{}
	}
}

func startFakeUpstream(t *testing.T, records map[string]netip.Addr) netip.AddrPort {
	t.Helper()
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	ap := pc.LocalAddr().(*net.UDPAddr).AddrPort()
	ln, err := net.ListenTCP("tcp", net.TCPAddrFromAddrPort(ap))
	if err != nil {
		pc.Close()
		t.Fatal(err)
	}
	answer := func(pkt []byte, maxUDP int) []byte {
		req, err := DecodeMessage(pkt)
		if err != nil || len(req.Questions) != 1 {
			return nil
		}
		q := req.Questions[0]
		resp := &Message{
			ID:                 req.ID,
			Response:           true,
			RecursionDesired:   req.RecursionDesired,
			RecursionAvailable: true,
			Questions:          req.Questions,
		}
		if ip, ok := records[strings.ToLower(q.Name)]; ok && q.Type == TypeA {
			resp.Answers = []Resource{{Name: q.Name, Type: TypeA, Class: ClassIN, TTL: 60, A: ip}}
		} else {
			resp.Rcode = RcodeNXDomain
		}
		wire, _ := EncodeMessage(resp, maxUDP)
		return wire
	}
	go func() {
		buf := make([]byte, MaxEDNSUDPSize)
		for {
			n, addr, err := pc.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			if wire := answer(buf[:n], MaxUDPSize); len(wire) > 0 {
				_, _ = pc.WriteToUDPAddrPort(wire, addr)
			}
		}
	}()
	go func() {
		for {
			c, err := ln.AcceptTCP()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var lb [2]byte
				if _, err := io.ReadFull(c, lb[:]); err != nil {
					return
				}
				pkt := make([]byte, binary.BigEndian.Uint16(lb[:]))
				if _, err := io.ReadFull(c, pkt); err != nil {
					return
				}
				wire := answer(pkt, 0)
				frame := make([]byte, 2+len(wire))
				binary.BigEndian.PutUint16(frame[0:2], uint16(len(wire)))
				copy(frame[2:], wire)
				_, _ = c.Write(frame)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = pc.Close()
		_ = ln.Close()
	})
	return ap
}
