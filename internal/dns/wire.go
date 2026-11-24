// Package dns implements Shearwater's in-process cluster DNS server
// (docs/design-multinode-tenancy.md 4.1). It speaks RFC 1035 wire format over
// UDP and TCP with no external DNS library.
package dns

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// DNS RR types, classes and response codes (RFC 1035, RFC 2782, RFC 3596, RFC 6891).
const (
	TypeA    uint16 = 1
	TypePTR  uint16 = 12
	TypeAAAA uint16 = 28
	TypeSRV  uint16 = 33
	TypeOPT  uint16 = 41
	TypeANY  uint16 = 255

	ClassIN  uint16 = 1
	ClassANY uint16 = 255

	OpcodeQuery uint8 = 0

	RcodeNoError  uint8 = 0
	RcodeFormErr  uint8 = 1
	RcodeServFail uint8 = 2
	RcodeNXDomain uint8 = 3
	RcodeNotImp   uint8 = 4
	RcodeRefused  uint8 = 5

	flagQR uint16 = 1 << 15
	flagAA uint16 = 1 << 10
	flagTC uint16 = 1 << 9
	flagRD uint16 = 1 << 8
	flagRA uint16 = 1 << 7

	headerLen      = 12
	MaxUDPSize     = 512
	MaxEDNSUDPSize = 4096
	maxNameLen     = 255
	maxLabelLen    = 63
	maxJumps       = 64
)

var (
	ErrTruncated   = errors.New("dns: packet is truncated")
	ErrBadName     = errors.New("dns: invalid domain name")
	ErrNameLoop    = errors.New("dns: compression pointer loop")
	ErrMessageHuge = errors.New("dns: message exceeds 65535 bytes")
)

// Message is one decoded DNS query or response.
type Message struct {
	ID                 uint16
	Response           bool
	Opcode             uint8
	Authoritative      bool
	Truncated          bool
	RecursionDesired   bool
	RecursionAvailable bool
	Rcode              uint8

	Questions   []Question
	Answers     []Resource
	Authorities []Resource
	Additionals []Resource
	EDNS        *EDNS0
}

// EDNS0 holds the fields we care about from an OPT pseudo-RR (RFC 6891).
type EDNS0 struct {
	UDPSize  uint16
	ExtRcode uint8
	Version  uint8
	DO       bool
}

// Question is one entry of the DNS question section.
type Question struct {
	Name  string // canonical dotted form, e.g. "web.default.svc.cluster.local." or "."
	Type  uint16
	Class uint16
}

// Resource is one DNS resource record.
type Resource struct {
	Name  string
	Type  uint16
	Class uint16
	TTL   uint32

	A   netip.Addr
	PTR string
	SRV *SRVRecord
	Raw []byte
}

// SRVRecord holds RFC 2782 SRV RDATA.
type SRVRecord struct {
	Priority uint16
	Weight   uint16
	Port     uint16
	Target   string
}

// DecodeMessage parses one RFC 1035 DNS wire packet. It bounds-checks every
// offset and pointer so malformed or truncated input returns an error and
// never panics.
func DecodeMessage(b []byte) (*Message, error) {
	if len(b) < headerLen {
		return nil, ErrTruncated
	}
	flags := binary.BigEndian.Uint16(b[2:4])
	m := &Message{
		ID:                 binary.BigEndian.Uint16(b[0:2]),
		Response:           flags&flagQR != 0,
		Opcode:             uint8((flags >> 11) & 0x0f),
		Authoritative:      flags&flagAA != 0,
		Truncated:          flags&flagTC != 0,
		RecursionDesired:   flags&flagRD != 0,
		RecursionAvailable: flags&flagRA != 0,
		Rcode:              uint8(flags & 0x0f),
	}
	qdCount := int(binary.BigEndian.Uint16(b[4:6]))
	anCount := int(binary.BigEndian.Uint16(b[6:8]))
	nsCount := int(binary.BigEndian.Uint16(b[8:10]))
	arCount := int(binary.BigEndian.Uint16(b[10:12]))

	// Reject absurd counts up front so a crafted 12-byte packet cannot allocate.
	if qdCount > len(b)/5 || anCount+nsCount+arCount > len(b)/11 {
		return nil, ErrTruncated
	}
	off := headerLen
	m.Questions = make([]Question, 0, qdCount)
	for range qdCount {
		name, next, err := decodeName(b, off)
		if err != nil {
			return nil, err
		}
		if next+4 > len(b) {
			return nil, ErrTruncated
		}
		m.Questions = append(m.Questions, Question{
			Name:  name,
			Type:  binary.BigEndian.Uint16(b[next : next+2]),
			Class: binary.BigEndian.Uint16(b[next+2 : next+4]),
		})
		off = next + 4
	}
	var err error
	if m.Answers, off, err = decodeResources(b, off, anCount, nil); err != nil {
		return nil, err
	}
	if m.Authorities, off, err = decodeResources(b, off, nsCount, nil); err != nil {
		return nil, err
	}
	if m.Additionals, off, err = decodeResources(b, off, arCount, &m.EDNS); err != nil {
		return nil, err
	}
	return m, nil
}

func decodeResources(b []byte, off, count int, edns **EDNS0) ([]Resource, int, error) {
	if count == 0 {
		return nil, off, nil
	}
	out := make([]Resource, 0, count)
	for range count {
		name, next, err := decodeName(b, off)
		if err != nil {
			return nil, off, err
		}
		if next+10 > len(b) {
			return nil, off, ErrTruncated
		}
		typ := binary.BigEndian.Uint16(b[next : next+2])
		class := binary.BigEndian.Uint16(b[next+2 : next+4])
		ttl := binary.BigEndian.Uint32(b[next+4 : next+8])
		rdlen := int(binary.BigEndian.Uint16(b[next+8 : next+10]))
		rdataOff := next + 10
		if rdataOff+rdlen > len(b) {
			return nil, off, ErrTruncated
		}
		rdata := b[rdataOff : rdataOff+rdlen]
		off = rdataOff + rdlen

		if typ == TypeOPT && edns != nil {
			if name != "." {
				return nil, off, ErrBadName
			}
			udpSize := class
			if udpSize < MaxUDPSize {
				udpSize = MaxUDPSize
			}
			*edns = &EDNS0{
				UDPSize:  udpSize,
				ExtRcode: uint8(ttl >> 24),
				Version:  uint8((ttl >> 16) & 0xff),
				DO:       ttl&0x8000 != 0,
			}
			continue
		}
		rr := Resource{Name: name, Type: typ, Class: class, TTL: ttl}
		switch typ {
		case TypeA:
			if rdlen != 4 {
				return nil, off, fmt.Errorf("dns: A record length %d, want 4", rdlen)
			}
			rr.A = netip.AddrFrom4([4]byte(rdata))
		case TypePTR:
			target, end, err := decodeName(b, rdataOff)
			if err != nil {
				return nil, off, err
			}
			if end != rdataOff+rdlen {
				return nil, off, ErrBadName
			}
			rr.PTR = target
		case TypeSRV:
			if rdlen < 7 {
				return nil, off, ErrTruncated
			}
			target, end, err := decodeName(b, rdataOff+6)
			if err != nil {
				return nil, off, err
			}
			if end != rdataOff+rdlen {
				return nil, off, ErrBadName
			}
			rr.SRV = &SRVRecord{
				Priority: binary.BigEndian.Uint16(rdata[0:2]),
				Weight:   binary.BigEndian.Uint16(rdata[2:4]),
				Port:     binary.BigEndian.Uint16(rdata[4:6]),
				Target:   target,
			}
		default:
			rr.Raw = append([]byte(nil), rdata...)
		}
		out = append(out, rr)
	}
	return out, off, nil
}

// decodeName reads a possibly compressed domain name starting at off.
// nextOff is always immediately after the name (or its first pointer) in b.
func decodeName(b []byte, off int) (name string, nextOff int, err error) {
	if off < 0 || off >= len(b) {
		return "", 0, ErrTruncated
	}
	var sb strings.Builder
	pos := off
	jumped := false
	jumps := 0
	wireLen := 0

	for {
		if pos >= len(b) {
			return "", 0, ErrTruncated
		}
		c := b[pos]
		switch c & 0xc0 {
		case 0:
			if c == 0 {
				wireLen++
				if wireLen > maxNameLen {
					return "", 0, ErrBadName
				}
				if !jumped {
					nextOff = pos + 1
				}
				if sb.Len() == 0 {
					return ".", nextOff, nil
				}
				return sb.String(), nextOff, nil
			}
			l := int(c)
			if l > maxLabelLen || pos+1+l > len(b) {
				return "", 0, ErrTruncated
			}
			wireLen += 1 + l
			if wireLen > maxNameLen {
				return "", 0, ErrBadName
			}
			sb.Write(b[pos+1 : pos+1+l])
			sb.WriteByte('.')
			pos += 1 + l
		case 0xc0:
			if pos+1 >= len(b) {
				return "", 0, ErrTruncated
			}
			ptr := int(c&0x3f)<<8 | int(b[pos+1])
			if ptr >= len(b) {
				return "", 0, ErrBadName
			}
			if !jumped {
				nextOff = pos + 2
				jumped = true
			}
			jumps++
			if jumps > maxJumps {
				return "", 0, ErrNameLoop
			}
			pos = ptr
		default:
			// 0x40 and 0x80 label types are reserved and unsupported in RFC 1035.
			return "", 0, ErrBadName
		}
	}
}

// EncodeMessage serializes m to RFC 1035 wire format with domain name
// compression. When maxUDP > 0 and the response exceeds maxUDP bytes, answers
// that do not fit are omitted and the TC bit is set. maxUDP == 0 encodes for
// TCP (up to 65535 bytes).
func EncodeMessage(m *Message, maxUDP int) ([]byte, error) {
	limit := 65535
	if maxUDP > 0 {
		limit = min(maxUDP, 65535)
	}
	// Reserve 11 bytes for an empty EDNS0 OPT RR when present.
	optReserve := 0
	if m.EDNS != nil {
		optReserve = 11
	}

	buf := make([]byte, headerLen, min(limit, 512))
	comp := map[string]int{}

	for _, q := range m.Questions {
		var err error
		buf, err = appendName(buf, q.Name, comp, true)
		if err != nil {
			return nil, err
		}
		buf = binary.BigEndian.AppendUint16(buf, q.Type)
		buf = binary.BigEndian.AppendUint16(buf, q.Class)
	}
	if len(buf) > limit {
		return nil, ErrMessageHuge
	}

	truncated := m.Truncated
	anCount := 0
	nsCount := 0
	arCount := 0

	appendSection := func(list []Resource, count *int, setTC bool) error {
		for _, rr := range list {
			mark := len(buf)
			// Snapshot compression entries added by this RR only if it fits.
			var err error
			buf, err = appendResource(buf, rr, comp)
			if err != nil {
				return err
			}
			if maxUDP > 0 && len(buf)+optReserve > limit {
				buf = buf[:mark]
				// Remove any compression offsets pointing at or past mark.
				for k, off := range comp {
					if off >= mark {
						delete(comp, k)
					}
				}
				if setTC {
					truncated = true
				}
				return nil
			}
			if len(buf) > 65535 {
				return ErrMessageHuge
			}
			*count++
		}
		return nil
	}

	if err := appendSection(m.Answers, &anCount, true); err != nil {
		return nil, err
	}
	if !truncated {
		if err := appendSection(m.Authorities, &nsCount, true); err != nil {
			return nil, err
		}
	}
	if !truncated {
		if err := appendSection(m.Additionals, &arCount, false); err != nil {
			return nil, err
		}
	}
	if m.EDNS != nil && len(buf)+optReserve <= limit {
		udpSize := m.EDNS.UDPSize
		if udpSize < MaxUDPSize {
			udpSize = MaxUDPSize
		}
		ttl := uint32(m.EDNS.ExtRcode)<<24 | uint32(m.EDNS.Version)<<16
		if m.EDNS.DO {
			ttl |= 0x8000
		}
		buf = append(buf, 0) // root name "."
		buf = binary.BigEndian.AppendUint16(buf, TypeOPT)
		buf = binary.BigEndian.AppendUint16(buf, udpSize)
		buf = binary.BigEndian.AppendUint32(buf, ttl)
		buf = binary.BigEndian.AppendUint16(buf, 0) // RDLENGTH = 0
		arCount++
	}

	var flags uint16
	if m.Response {
		flags |= flagQR
	}
	flags |= uint16(m.Opcode&0x0f) << 11
	if m.Authoritative {
		flags |= flagAA
	}
	if truncated {
		flags |= flagTC
	}
	if m.RecursionDesired {
		flags |= flagRD
	}
	if m.RecursionAvailable {
		flags |= flagRA
	}
	flags |= uint16(m.Rcode & 0x0f)

	binary.BigEndian.PutUint16(buf[0:2], m.ID)
	binary.BigEndian.PutUint16(buf[2:4], flags)
	binary.BigEndian.PutUint16(buf[4:6], uint16(len(m.Questions)))
	binary.BigEndian.PutUint16(buf[6:8], uint16(anCount))
	binary.BigEndian.PutUint16(buf[8:10], uint16(nsCount))
	binary.BigEndian.PutUint16(buf[10:12], uint16(arCount))
	return buf, nil
}

func appendResource(buf []byte, rr Resource, comp map[string]int) ([]byte, error) {
	var err error
	buf, err = appendName(buf, rr.Name, comp, true)
	if err != nil {
		return nil, err
	}
	buf = binary.BigEndian.AppendUint16(buf, rr.Type)
	class := rr.Class
	if class == 0 {
		class = ClassIN
	}
	buf = binary.BigEndian.AppendUint16(buf, class)
	buf = binary.BigEndian.AppendUint32(buf, rr.TTL)
	rdlenPos := len(buf)
	buf = append(buf, 0, 0)
	rdataStart := len(buf)

	switch rr.Type {
	case TypeA:
		if !rr.A.Is4() {
			return nil, fmt.Errorf("dns: A record requires an IPv4 address, got %v", rr.A)
		}
		a4 := rr.A.As4()
		buf = append(buf, a4[:]...)
	case TypePTR:
		buf, err = appendName(buf, rr.PTR, comp, true)
		if err != nil {
			return nil, err
		}
	case TypeSRV:
		if rr.SRV == nil {
			return nil, errors.New("dns: SRV record is nil")
		}
		buf = binary.BigEndian.AppendUint16(buf, rr.SRV.Priority)
		buf = binary.BigEndian.AppendUint16(buf, rr.SRV.Weight)
		buf = binary.BigEndian.AppendUint16(buf, rr.SRV.Port)
		// RFC 2782: SRV target must not use name compression.
		buf, err = appendName(buf, rr.SRV.Target, comp, false)
		if err != nil {
			return nil, err
		}
	default:
		buf = append(buf, rr.Raw...)
	}

	rdlen := len(buf) - rdataStart
	if rdlen > 65535 {
		return nil, ErrMessageHuge
	}
	binary.BigEndian.PutUint16(buf[rdlenPos:rdlenPos+2], uint16(rdlen))
	return buf, nil
}

// appendName writes a domain name in wire format, optionally compressing
// suffixes against offsets already present in comp.
func appendName(buf []byte, name string, comp map[string]int, compress bool) ([]byte, error) {
	name = strings.TrimSuffix(name, ".")
	if name == "" {
		return append(buf, 0), nil
	}
	labels := strings.Split(name, ".")
	wireLen := 1
	for _, l := range labels {
		if len(l) == 0 || len(l) > maxLabelLen {
			return nil, ErrBadName
		}
		wireLen += 1 + len(l)
	}
	if wireLen > maxNameLen {
		return nil, ErrBadName
	}

	for i := range labels {
		suffix := strings.ToLower(strings.Join(labels[i:], ".")) + "."
		if compress {
			if off, ok := comp[suffix]; ok && off < 0x4000 {
				buf = append(buf, byte(0xc0|(off>>8)), byte(off&0xff))
				return buf, nil
			}
		}
		if len(buf) < 0x4000 {
			if _, exists := comp[suffix]; !exists {
				comp[suffix] = len(buf)
			}
		}
		l := labels[i]
		buf = append(buf, byte(len(l)))
		buf = append(buf, l...)
	}
	return append(buf, 0), nil
}

// CanonicalName returns a lowercase FQDN with a single trailing dot.
func CanonicalName(name string) string {
	name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	if name == "" {
		return "."
	}
	return name + "."
}
