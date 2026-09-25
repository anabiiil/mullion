package dnsd

import (
	"encoding/binary"
	"fmt"
	"net"
	"testing"
	"time"
)

func header(t *testing.T, resp []byte) (id, flags, qd, an, ns, ar uint16) {
	t.Helper()
	if len(resp) < headerLen {
		t.Fatalf("response too short: %d bytes", len(resp))
	}
	u := func(i int) uint16 { return binary.BigEndian.Uint16(resp[i:]) }
	return u(0), u(2), u(4), u(6), u(8), u(10)
}

func TestHandleAnswersA(t *testing.T) {
	q := BuildQuery(0x1234, "Tenant1.Shop.TEST", TypeA)
	resp := Handle(q, "test")
	id, flags, qd, an, ns, ar := header(t, resp)
	if id != 0x1234 {
		t.Fatalf("id = %#x", id)
	}
	if flags&flagQR == 0 || flags&flagAA == 0 || flags&flagRD == 0 || flags&0xF != RcodeSuccess {
		t.Fatalf("flags = %#x, want QR|AA|RD NOERROR", flags)
	}
	if qd != 1 || an != 1 || ns != 0 || ar != 0 {
		t.Fatalf("counts qd=%d an=%d ns=%d ar=%d", qd, an, ns, ar)
	}
	// The question is echoed verbatim (original casing).
	if string(resp[headerLen:len(q)]) != string(q[headerLen:]) {
		t.Fatalf("question not echoed verbatim")
	}
	ips, rcode, err := ParseAnswers(resp, 0x1234)
	if err != nil || rcode != 0 || len(ips) != 1 || !ips[0].Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("answers = %v rcode=%d err=%v", ips, rcode, err)
	}
	// The answer owner is a pointer to the question name.
	if resp[len(q)] != 0xC0 || resp[len(q)+1] != headerLen {
		t.Fatalf("answer owner is not a pointer to offset 12: % x", resp[len(q):len(q)+2])
	}
}

func TestHandleAnswersAAAAandANY(t *testing.T) {
	resp := Handle(BuildQuery(7, "shop.test", TypeAAAA), "test")
	ips, _, err := ParseAnswers(resp, 7)
	if err != nil || len(ips) != 1 || !ips[0].Equal(net.IPv6loopback) {
		t.Fatalf("AAAA answers = %v err=%v", ips, err)
	}
	resp = Handle(BuildQuery(8, "a.b.shop.test", TypeANY), "test")
	ips, _, err = ParseAnswers(resp, 8)
	if err != nil || len(ips) != 2 {
		t.Fatalf("ANY answers = %v err=%v", ips, err)
	}
}

func TestHandleNoDataCarriesSOA(t *testing.T) {
	resp := Handle(BuildQuery(9, "shop.test", 15 /* MX */), "test")
	_, flags, _, an, ns, _ := header(t, resp)
	if flags&0xF != RcodeSuccess || an != 0 || ns != 1 || flags&flagAA == 0 {
		t.Fatalf("NODATA: flags=%#x an=%d ns=%d", flags, an, ns)
	}
	// Apex A query: NODATA too.
	resp = Handle(BuildQuery(10, "test", TypeA), "test")
	_, flags, _, an, ns, _ = header(t, resp)
	if flags&0xF != RcodeSuccess || an != 0 || ns != 1 {
		t.Fatalf("apex: flags=%#x an=%d ns=%d", flags, an, ns)
	}
	// Apex SOA query: answered.
	resp = Handle(BuildQuery(11, "test", TypeSOA), "test")
	_, _, _, an, _, _ = header(t, resp)
	if an != 1 {
		t.Fatalf("apex SOA: an=%d", an)
	}
}

func TestHandleRefusesOutOfZone(t *testing.T) {
	for _, name := range []string{"example.com", "nottest", "test.com", "shop.testing"} {
		resp := Handle(BuildQuery(1, name, TypeA), "test")
		_, flags, qd, an, _, _ := header(t, resp)
		if flags&0xF != RcodeRefused || an != 0 || qd != 1 || flags&flagAA != 0 {
			t.Fatalf("%s: flags=%#x an=%d qd=%d, want REFUSED", name, flags, an, qd)
		}
	}
	// No zone configured: refuse everything.
	resp := Handle(BuildQuery(1, "shop.test", TypeA), "")
	if _, flags, _, _, _, _ := header(t, resp); flags&0xF != RcodeRefused {
		t.Fatalf("empty zone must refuse, flags=%#x", flags)
	}
}

func TestHandleMalformedAndDrops(t *testing.T) {
	if Handle([]byte{1, 2, 3}, "test") != nil {
		t.Fatal("a sub-header packet must be dropped")
	}
	// A response (QR set) must never be answered (no reflection loops).
	resp := BuildQuery(1, "a.test", TypeA)
	resp[2] |= 0x80
	if Handle(resp, "test") != nil {
		t.Fatal("responses must be dropped")
	}
	// Truncated question -> FORMERR with the same ID.
	q := BuildQuery(0xBEEF, "a.test", TypeA)
	out := Handle(q[:len(q)-3], "test")
	id, flags, qd, _, _, _ := header(t, out)
	if id != 0xBEEF || flags&0xF != RcodeFormErr || qd != 0 {
		t.Fatalf("FORMERR: id=%#x flags=%#x qd=%d", id, flags, qd)
	}
	// Compression pointer inside the question -> FORMERR.
	bad := append([]byte{}, q[:headerLen]...)
	bad = append(bad, 0xC0, 0x0C, 0, 1, 0, 1)
	if _, flags, _, _, _, _ := header(t, Handle(bad, "test")); flags&0xF != RcodeFormErr {
		t.Fatalf("pointer in query must FORMERR, flags=%#x", flags)
	}
	// Two questions -> FORMERR.
	two := append([]byte{}, q...)
	two[5] = 2
	if _, flags, _, _, _, _ := header(t, Handle(two, "test")); flags&0xF != RcodeFormErr {
		t.Fatalf("qdcount=2 must FORMERR, flags=%#x", flags)
	}
	// Non-query opcode (NOTIFY=4) -> NOTIMP.
	notify := append([]byte{}, q...)
	notify[2] |= 4 << 3
	if _, flags, _, _, _, _ := header(t, Handle(notify, "test")); flags&0xF != RcodeNotImp {
		t.Fatalf("opcode 4 must NOTIMP, flags=%#x", flags)
	}
}

func withOPT(q []byte, version uint8) []byte {
	out := append([]byte{}, q...)
	out[11] = 1 // arcount
	out = append(out, 0)
	out = binary.BigEndian.AppendUint16(out, TypeOPT)
	out = binary.BigEndian.AppendUint16(out, 4096)
	out = binary.BigEndian.AppendUint32(out, uint32(version)<<16)
	return binary.BigEndian.AppendUint16(out, 0)
}

func TestHandleEDNS(t *testing.T) {
	resp := Handle(withOPT(BuildQuery(5, "x.shop.test", TypeA), 0), "test")
	_, flags, _, an, _, ar := header(t, resp)
	if flags&0xF != 0 || an != 1 || ar != 1 {
		t.Fatalf("EDNS query: flags=%#x an=%d ar=%d", flags, an, ar)
	}
	opt := resp[len(resp)-11:]
	if opt[0] != 0 || binary.BigEndian.Uint16(opt[1:]) != TypeOPT || binary.BigEndian.Uint16(opt[3:]) != ednsUDPSize {
		t.Fatalf("bad OPT record: % x", opt)
	}
	// Unknown EDNS version -> BADVERS (16): header rcode 0, ext rcode 1.
	resp = Handle(withOPT(BuildQuery(6, "x.shop.test", TypeA), 1), "test")
	_, flags, _, an, _, ar = header(t, resp)
	opt = resp[len(resp)-11:]
	if flags&0xF != 0 || an != 0 || ar != 1 || opt[5] != 1 {
		t.Fatalf("BADVERS: flags=%#x an=%d ar=%d ext=%d", flags, an, ar, opt[5])
	}
}

func TestServerUDPAndTCPRoundTrip(t *testing.T) {
	// Pick a free high port for both protocols.
	var addr string
	var s *Server
	for i := 0; i < 20; i++ {
		l, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr = l.LocalAddr().String()
		l.Close()
		s = &Server{Zone: func() string { return "test" }}
		if err := s.Listen(addr); err == nil {
			break
		}
		s = nil
	}
	if s == nil {
		t.Fatal("could not bind a test port")
	}
	defer s.Close()

	if err := Probe(addr, "tenant1.shop.test", 2*time.Second); err != nil {
		t.Fatalf("UDP probe: %v", err)
	}
	if err := Probe(addr, "example.com", 2*time.Second); err == nil {
		t.Fatal("out-of-zone probe must fail")
	}

	// TCP: two queries on one connection, length-prefixed.
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	for i := uint16(1); i <= 2; i++ {
		q := BuildQuery(i, fmt.Sprintf("t%d.shop.test", i), TypeA)
		c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(q))), q...))
		var lb [2]byte
		if _, err := c.Read(lb[:]); err != nil {
			t.Fatal(err)
		}
		resp := make([]byte, binary.BigEndian.Uint16(lb[:]))
		if _, err := ioReadFull(c, resp); err != nil {
			t.Fatal(err)
		}
		ips, _, err := ParseAnswers(resp, i)
		if err != nil || len(ips) != 1 {
			t.Fatalf("TCP answer %d: %v %v", i, ips, err)
		}
	}
}

func ioReadFull(c net.Conn, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := c.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
