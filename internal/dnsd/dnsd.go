// Package dnsd is Mullion's tiny authoritative DNS server for the local
// TLD. A hosts file can't express "*.shop.test", so when wildcard DNS is
// on, the OS resolver forwards every *.<tld> lookup here (macOS:
// /etc/resolver/<tld>; Windows: an NRPT rule) and every name under the
// TLD answers loopback. Standard library only: UDP + TCP, one question
// per query, minimal EDNS.
package dnsd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Port is where the server listens on loopback. macOS's /etc/resolver
// files can name a port; Windows' NRPT cannot, so there the server also
// tries 127.0.0.1:53 (see ListenAddrs).
const Port = 53535

// TTL of every answer, in seconds: short, so a TLD change or disabling
// wildcard DNS takes effect quickly.
const TTL = 60

// DNS wire constants (RFC 1035, RFC 6891).
const (
	TypeA    uint16 = 1
	TypeNS   uint16 = 2
	TypeSOA  uint16 = 6
	TypeAAAA uint16 = 28
	TypeOPT  uint16 = 41
	TypeANY  uint16 = 255

	ClassIN  uint16 = 1
	ClassANY uint16 = 255

	RcodeSuccess  = 0
	RcodeFormErr  = 1
	RcodeServFail = 2
	RcodeNXDomain = 3
	RcodeNotImp   = 4
	RcodeRefused  = 5
	rcodeBadVers  = 16 // extended (EDNS) rcode

	flagQR = 1 << 15
	flagAA = 1 << 10
	flagTC = 1 << 9
	flagRD = 1 << 8

	headerLen   = 12
	maxNameLen  = 255
	ednsUDPSize = 1232
)

// ListenAddrs are the loopback addresses the server should bind on this
// OS. Windows adds 127.0.0.1:53 because an NRPT rule can't carry a port.
func ListenAddrs() []string {
	addrs := []string{fmt.Sprintf("127.0.0.1:%d", Port), fmt.Sprintf("[::1]:%d", Port)}
	if runtime.GOOS == "windows" {
		addrs = append(addrs, "127.0.0.1:53")
	}
	return addrs
}

// ProbeAddr is the address the OS resolver hookup points at, i.e. what a
// health check should query.
func ProbeAddr() string {
	if runtime.GOOS == "windows" {
		return "127.0.0.1:53"
	}
	return fmt.Sprintf("127.0.0.1:%d", Port)
}

// question is a parsed query question. raw is its exact wire bytes
// (name + type + class) so the response echoes the asker's casing.
type question struct {
	name  string // lowercased, no trailing dot
	qtype uint16
	class uint16
	raw   []byte
}

type edns struct {
	present bool
	version uint8
}

var errFormat = errors.New("malformed DNS message")

// readName decodes an uncompressed domain name at off. Queries never
// need compression, and refusing pointers keeps parsing loop-free.
func readName(msg []byte, off int) (string, int, error) {
	var labels []string
	total := 0
	for {
		if off >= len(msg) {
			return "", 0, errFormat
		}
		l := int(msg[off])
		off++
		if l == 0 {
			break
		}
		if l&0xC0 != 0 { // compression pointer or reserved label type
			return "", 0, errFormat
		}
		if off+l > len(msg) {
			return "", 0, errFormat
		}
		total += l + 1
		if total > maxNameLen {
			return "", 0, errFormat
		}
		labels = append(labels, string(msg[off:off+l]))
		off += l
	}
	return strings.ToLower(strings.Join(labels, ".")), off, nil
}

// parse decodes the header, the single question and any OPT record.
func parse(msg []byte) (id, flags uint16, q question, e edns, err error) {
	if len(msg) < headerLen {
		return 0, 0, q, e, errFormat
	}
	id = binary.BigEndian.Uint16(msg[0:])
	flags = binary.BigEndian.Uint16(msg[2:])
	qd := binary.BigEndian.Uint16(msg[4:])
	an := binary.BigEndian.Uint16(msg[6:])
	ns := binary.BigEndian.Uint16(msg[8:])
	ar := binary.BigEndian.Uint16(msg[10:])
	if qd != 1 {
		return id, flags, q, e, errFormat
	}
	off := headerLen
	name, next, err := readName(msg, off)
	if err != nil || next+4 > len(msg) {
		return id, flags, q, e, errFormat
	}
	q = question{
		name:  name,
		qtype: binary.BigEndian.Uint16(msg[next:]),
		class: binary.BigEndian.Uint16(msg[next+2:]),
		raw:   msg[off : next+4],
	}
	off = next + 4
	// Skip any answer/authority records (not expected in a query), then
	// look for the OPT pseudo-record among the additional ones.
	for i := 0; i < int(an)+int(ns)+int(ar); i++ {
		_, n, err := readName(msg, off)
		if err != nil || n+10 > len(msg) {
			return id, flags, q, e, errFormat
		}
		rtype := binary.BigEndian.Uint16(msg[n:])
		ttl := binary.BigEndian.Uint32(msg[n+4:])
		rdlen := int(binary.BigEndian.Uint16(msg[n+8:]))
		if n+10+rdlen > len(msg) {
			return id, flags, q, e, errFormat
		}
		if i >= int(an)+int(ns) && rtype == TypeOPT {
			if e.present { // more than one OPT is a FORMERR (RFC 6891 6.1.1)
				return id, flags, q, e, errFormat
			}
			e = edns{present: true, version: uint8(ttl >> 16)}
		}
		off = n + 10 + rdlen
	}
	return id, flags, q, e, nil
}

// inZone reports whether name is the zone apex or under it.
func inZone(name, zone string) (under, apex bool) {
	if zone == "" {
		return false, false
	}
	if name == zone {
		return false, true
	}
	return strings.HasSuffix(name, "."+zone), false
}

// Handle answers one query for zone (the TLD, e.g. "test"). It returns
// nil when the message must be dropped (too short to carry an ID, or it
// is itself a response).
func Handle(msg []byte, zone string) []byte {
	zone = strings.Trim(strings.ToLower(zone), ".")
	if len(msg) < headerLen {
		return nil
	}
	id, flags, q, e, err := parse(msg)
	if flags&flagQR != 0 {
		return nil
	}
	opcode := (flags >> 11) & 0xF
	w := &writer{id: id, flags: flagQR | (flags & (0xF << 11)) | (flags & flagRD)}
	if err != nil {
		w.rcode = RcodeFormErr
		return w.bytes()
	}
	w.q = &q
	w.edns = e.present
	switch {
	case opcode != 0:
		w.rcode = RcodeNotImp
		return w.bytes()
	case e.present && e.version != 0:
		w.rcode = rcodeBadVers
		return w.bytes()
	case q.class != ClassIN && q.class != ClassANY:
		w.rcode = RcodeRefused
		return w.bytes()
	}
	under, apex := inZone(q.name, zone)
	if !under && !apex {
		w.rcode = RcodeRefused
		return w.bytes()
	}
	w.flags |= flagAA
	if apex {
		if q.qtype == TypeSOA || q.qtype == TypeANY {
			w.answers = append(w.answers, soaRecord(zone, true))
		} else {
			w.authority = append(w.authority, soaRecord(zone, false))
		}
		return w.bytes()
	}
	if q.qtype == TypeA || q.qtype == TypeANY {
		w.answers = append(w.answers, addrRecord(TypeA, net.IPv4(127, 0, 0, 1).To4()))
	}
	if q.qtype == TypeAAAA || q.qtype == TypeANY {
		w.answers = append(w.answers, addrRecord(TypeAAAA, net.IPv6loopback))
	}
	if len(w.answers) == 0 {
		// NOERROR/NODATA: the name exists, not this type. The SOA lets
		// resolvers cache the negative answer.
		w.authority = append(w.authority, soaRecord(zone, false))
	}
	return w.bytes()
}

// addrRecord is an A/AAAA answer whose owner is a compression pointer to
// the question name (always at offset 12).
func addrRecord(t uint16, ip net.IP) []byte {
	b := []byte{0xC0, headerLen}
	b = binary.BigEndian.AppendUint16(b, t)
	b = binary.BigEndian.AppendUint16(b, ClassIN)
	b = binary.BigEndian.AppendUint32(b, TTL)
	b = binary.BigEndian.AppendUint16(b, uint16(len(ip)))
	return append(b, ip...)
}

// soaRecord is the zone's synthetic SOA. At the apex (question name ==
// zone) the owner can point at the question; otherwise it is spelled out.
func soaRecord(zone string, ownerIsQuestion bool) []byte {
	var b []byte
	if ownerIsQuestion {
		b = []byte{0xC0, headerLen}
	} else {
		b = appendName(nil, zone)
	}
	rdata := appendName(nil, "ns.mullion."+zone)
	rdata = appendName(rdata, "hostmaster.mullion."+zone)
	for _, v := range []uint32{1, 3600, 600, 86400, TTL} { // serial refresh retry expire minimum
		rdata = binary.BigEndian.AppendUint32(rdata, v)
	}
	b = binary.BigEndian.AppendUint16(b, TypeSOA)
	b = binary.BigEndian.AppendUint16(b, ClassIN)
	b = binary.BigEndian.AppendUint32(b, TTL)
	b = binary.BigEndian.AppendUint16(b, uint16(len(rdata)))
	return append(b, rdata...)
}

func appendName(b []byte, name string) []byte {
	for _, l := range strings.Split(strings.Trim(name, "."), ".") {
		if l == "" {
			continue
		}
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	return append(b, 0)
}

type writer struct {
	id, flags uint16
	rcode     int
	q         *question
	answers   [][]byte
	authority [][]byte
	edns      bool
}

func (w *writer) bytes() []byte {
	// Extended rcodes (BADVERS) only fit through OPT: the low 4 bits go
	// in the header, the high 8 in the OPT TTL.
	if w.rcode > 0xF {
		w.edns = true
	}
	flags := w.flags | uint16(w.rcode&0xF)
	b := binary.BigEndian.AppendUint16(nil, w.id)
	b = binary.BigEndian.AppendUint16(b, flags)
	qd := 0
	if w.q != nil {
		qd = 1
	}
	ar := 0
	if w.edns {
		ar = 1
	}
	b = binary.BigEndian.AppendUint16(b, uint16(qd))
	b = binary.BigEndian.AppendUint16(b, uint16(len(w.answers)))
	b = binary.BigEndian.AppendUint16(b, uint16(len(w.authority)))
	b = binary.BigEndian.AppendUint16(b, uint16(ar))
	if w.q != nil {
		b = append(b, w.q.raw...)
	}
	for _, rr := range w.answers {
		b = append(b, rr...)
	}
	for _, rr := range w.authority {
		b = append(b, rr...)
	}
	if w.edns {
		b = append(b, 0) // root owner
		b = binary.BigEndian.AppendUint16(b, TypeOPT)
		b = binary.BigEndian.AppendUint16(b, ednsUDPSize)
		b = binary.BigEndian.AppendUint32(b, uint32(w.rcode>>4)<<24) // ext rcode, version 0, no flags
		b = binary.BigEndian.AppendUint16(b, 0)
	}
	return b
}

// BuildQuery encodes a standard recursive query for one name/type — used
// by Probe and the tests.
func BuildQuery(id uint16, name string, qtype uint16) []byte {
	b := binary.BigEndian.AppendUint16(nil, id)
	b = binary.BigEndian.AppendUint16(b, flagRD)
	b = binary.BigEndian.AppendUint16(b, 1)
	b = append(b, 0, 0, 0, 0, 0, 0)
	b = appendName(b, name)
	b = binary.BigEndian.AppendUint16(b, qtype)
	return binary.BigEndian.AppendUint16(b, ClassIN)
}

// Server serves Handle over UDP and TCP on one or more addresses.
type Server struct {
	// Zone returns the TLD to answer for, read on every query so a TLD
	// change needs no restart. "" refuses everything.
	Zone func() string

	mu    sync.Mutex
	conns []net.PacketConn
	lns   []net.Listener
}

// Listen binds addr (UDP and TCP) and serves it in the background. A
// failure to bind either protocol releases the other and is returned.
func (s *Server) Listen(addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		pc.Close()
		return err
	}
	s.mu.Lock()
	s.conns = append(s.conns, pc)
	s.lns = append(s.lns, ln)
	s.mu.Unlock()
	go s.serveUDP(pc)
	go s.serveTCP(ln)
	return nil
}

// Close stops every listener.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		c.Close()
	}
	for _, l := range s.lns {
		l.Close()
	}
	s.conns, s.lns = nil, nil
	return nil
}

func (s *Server) zone() string {
	if s.Zone == nil {
		return ""
	}
	return s.Zone()
}

func (s *Server) serveUDP(pc net.PacketConn) {
	buf := make([]byte, 65535)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if resp := Handle(buf[:n], s.zone()); resp != nil {
			if len(resp) > 512 {
				// Never happens with one A/AAAA pair, but stay correct:
				// truncate to the header and let the client retry on TCP.
				resp = append(resp[:2:2], resp[2]|byte(flagTC>>8), resp[3], 0, 0, 0, 0, 0, 0, 0, 0)
			}
			_, _ = pc.WriteTo(resp, from)
		}
	}
}

func (s *Server) serveTCP(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go s.serveConn(c)
	}
}

func (s *Server) serveConn(c net.Conn) {
	defer c.Close()
	var lenBuf [2]byte
	for {
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.ReadFull(c, lenBuf[:]); err != nil {
			return
		}
		msg := make([]byte, binary.BigEndian.Uint16(lenBuf[:]))
		if _, err := io.ReadFull(c, msg); err != nil {
			return
		}
		resp := Handle(msg, s.zone())
		if resp == nil {
			return
		}
		out := binary.BigEndian.AppendUint16(nil, uint16(len(resp)))
		if _, err := c.Write(append(out, resp...)); err != nil {
			return
		}
	}
}

// Probe asks the server at addr (UDP) for name's A record and reports
// whether it answered 127.0.0.1.
func Probe(addr, name string, timeout time.Duration) error {
	conn, err := net.DialTimeout("udp", addr, timeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	const id = 0x4d4c // "ML"
	if _, err := conn.Write(BuildQuery(id, name, TypeA)); err != nil {
		return err
	}
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("no answer from %s: %w", addr, err)
	}
	ips, rcode, err := ParseAnswers(buf[:n], id)
	if err != nil {
		return err
	}
	if rcode != RcodeSuccess {
		return fmt.Errorf("%s answered rcode %d for %s", addr, rcode, name)
	}
	for _, ip := range ips {
		if ip.Equal(net.IPv4(127, 0, 0, 1)) {
			return nil
		}
	}
	return fmt.Errorf("%s did not answer 127.0.0.1 for %s", addr, name)
}

// ParseAnswers extracts the A/AAAA addresses and rcode from a response
// to a query with the given id (answer owners may be compressed).
func ParseAnswers(msg []byte, id uint16) ([]net.IP, int, error) {
	if len(msg) < headerLen || binary.BigEndian.Uint16(msg) != id {
		return nil, 0, errors.New("unexpected DNS response")
	}
	flags := binary.BigEndian.Uint16(msg[2:])
	if flags&flagQR == 0 {
		return nil, 0, errors.New("not a DNS response")
	}
	qd := int(binary.BigEndian.Uint16(msg[4:]))
	an := int(binary.BigEndian.Uint16(msg[6:]))
	off := headerLen
	for i := 0; i < qd; i++ {
		n, err := skipName(msg, off)
		if err != nil || n+4 > len(msg) {
			return nil, 0, errFormat
		}
		off = n + 4
	}
	var ips []net.IP
	for i := 0; i < an; i++ {
		n, err := skipName(msg, off)
		if err != nil || n+10 > len(msg) {
			return nil, 0, errFormat
		}
		t := binary.BigEndian.Uint16(msg[n:])
		rdlen := int(binary.BigEndian.Uint16(msg[n+8:]))
		if n+10+rdlen > len(msg) {
			return nil, 0, errFormat
		}
		rdata := msg[n+10 : n+10+rdlen]
		if (t == TypeA && rdlen == 4) || (t == TypeAAAA && rdlen == 16) {
			ips = append(ips, net.IP(append([]byte(nil), rdata...)))
		}
		off = n + 10 + rdlen
	}
	return ips, int(flags & 0xF), nil
}

// skipName steps over a possibly-compressed name, returning the offset
// just past it in the original message.
func skipName(msg []byte, off int) (int, error) {
	for {
		if off >= len(msg) {
			return 0, errFormat
		}
		l := int(msg[off])
		switch {
		case l == 0:
			return off + 1, nil
		case l&0xC0 == 0xC0:
			if off+2 > len(msg) {
				return 0, errFormat
			}
			return off + 2, nil
		case l&0xC0 != 0:
			return 0, errFormat
		}
		off += 1 + l
	}
}
