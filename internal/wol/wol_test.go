package wol

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func mustMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	mac, err := ParseMAC(s)
	if err != nil {
		t.Fatalf("ParseMAC(%q): %v", s, err)
	}
	return mac
}

func TestBuildPacketLayout(t *testing.T) {
	mac := mustMAC(t, "aa:bb:cc:dd:ee:ff")

	packet, err := BuildPacket(mac, nil)
	if err != nil {
		t.Fatalf("BuildPacket: %v", err)
	}
	if len(packet) != PacketLen {
		t.Fatalf("packet length = %d, want %d", len(packet), PacketLen)
	}
	if !bytes.Equal(packet[:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}) {
		t.Fatalf("packet must open with six 0xff bytes, got % x", packet[:6])
	}
	for i := range 16 {
		got := packet[6+i*6 : 12+i*6]
		if !bytes.Equal(got, mac) {
			t.Fatalf("repetition %d = % x, want % x", i, got, mac)
		}
	}
}

func TestBuildPacketWithPassword(t *testing.T) {
	mac := mustMAC(t, "00:11:22:33:44:55")
	password := []byte{1, 2, 3, 4, 5, 6}

	packet, err := BuildPacket(mac, password)
	if err != nil {
		t.Fatalf("BuildPacket: %v", err)
	}
	if len(packet) != PacketLen+6 {
		t.Fatalf("packet length = %d, want %d", len(packet), PacketLen+6)
	}
	if !bytes.Equal(packet[PacketLen:], password) {
		t.Fatalf("password trailer = % x, want % x", packet[PacketLen:], password)
	}
}

func TestBuildPacketRejectsBadInput(t *testing.T) {
	mac := mustMAC(t, "00:11:22:33:44:55")

	if _, err := BuildPacket(net.HardwareAddr{1, 2, 3}, nil); err == nil {
		t.Fatal("a 3-byte address must be rejected")
	}
	if _, err := BuildPacket(mac, []byte{1, 2, 3}); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("short password error = %v, want ErrInvalidPassword", err)
	}
}

func TestParseMAC(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		fails bool
	}{
		{name: "colons", input: "aa:bb:cc:dd:ee:ff", want: "aa:bb:cc:dd:ee:ff"},
		{name: "dashes", input: "AA-BB-CC-DD-EE-FF", want: "aa:bb:cc:dd:ee:ff"},
		{name: "padded", input: "  aa:bb:cc:dd:ee:ff  ", want: "aa:bb:cc:dd:ee:ff"},
		{name: "infiniband", input: "00:00:00:00:fe:80:00:00:00:00:00:00:02:00:5e:10:00:00:00:01", fails: true},
		{name: "nonsense", input: "not-a-mac", fails: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseMAC(tc.input)
			if tc.fails {
				if err == nil {
					t.Fatalf("ParseMAC(%q) = %v, want an error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMAC(%q): %v", tc.input, err)
			}
			if got.String() != tc.want {
				t.Fatalf("ParseMAC(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestParsePassword(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []byte
		fails bool
	}{
		{name: "empty", input: "", want: nil},
		{name: "whitespace only", input: "  \n", want: nil},
		{name: "hex with colons", input: "01:02:03:04:05:06", want: []byte{1, 2, 3, 4, 5, 6}},
		{name: "hex with dashes", input: "01-02-03-04-05-06", want: []byte{1, 2, 3, 4, 5, 6}},
		{name: "bare hex", input: "0a0b0c0d0e0f", want: []byte{10, 11, 12, 13, 14, 15}},
		{name: "trailing newline", input: "0a0b0c0d0e0f\n", want: []byte{10, 11, 12, 13, 14, 15}},
		{name: "six raw bytes", input: "secret", want: []byte("secret")},
		{name: "too short", input: "0a0b", fails: true},
		{name: "too long", input: "0a0b0c0d0e0f10", fails: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePassword([]byte(tc.input))
			if tc.fails {
				if err == nil {
					t.Fatalf("ParsePassword(%q) = % x, want an error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePassword(%q): %v", tc.input, err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("ParsePassword(%q) = % x, want % x", tc.input, got, tc.want)
			}
		})
	}
}

// A six-character password made only of hex digits is ambiguous. Decoding it
// as hex would yield three bytes, which is not a valid password, so the raw
// reading is the only one that can be right.
func TestParsePasswordPrefersSixRawBytesOverAmbiguousHex(t *testing.T) {
	got, err := ParsePassword([]byte("abcdef"))
	if err != nil {
		t.Fatalf("ParsePassword: %v", err)
	}
	if !bytes.Equal(got, []byte("abcdef")) {
		t.Fatalf("got % x, want the six raw bytes", got)
	}
}

// fakeConn records everything written to it.
type fakeConn struct {
	net.Conn
	writes [][]byte
	err    error
}

func (c *fakeConn) Write(b []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	c.writes = append(c.writes, bytes.Clone(b))
	return len(b), nil
}
func (c *fakeConn) Close() error                       { return nil }
func (c *fakeConn) SetWriteDeadline(_ time.Time) error { return nil }

type fakeDialer struct {
	conn      *fakeConn
	addresses []string
	err       error
}

func (d *fakeDialer) Dial(_ context.Context, address string) (net.Conn, error) {
	d.addresses = append(d.addresses, address)
	if d.err != nil {
		return nil, d.err
	}
	return d.conn, nil
}

func TestSendWritesEveryPacket(t *testing.T) {
	dialer := &fakeDialer{conn: &fakeConn{}}
	sender := NewSenderWithDialer(dialer)

	sent, err := sender.Send(context.Background(), &Target{
		MAC:       mustMAC(t, "aa:bb:cc:dd:ee:ff"),
		Broadcast: "192.168.1.255",
		Port:      9,
		Count:     3,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sent != 3 {
		t.Fatalf("sent = %d, want 3", sent)
	}
	if len(dialer.conn.writes) != 3 {
		t.Fatalf("wrote %d packets, want 3", len(dialer.conn.writes))
	}
	if got := dialer.addresses[0]; got != "192.168.1.255:9" {
		t.Fatalf("dialed %q, want %q", got, "192.168.1.255:9")
	}
	for i, w := range dialer.conn.writes {
		if len(w) != PacketLen {
			t.Fatalf("packet %d has length %d, want %d", i, len(w), PacketLen)
		}
	}
}

func TestSendDefaultsToOnePacket(t *testing.T) {
	dialer := &fakeDialer{conn: &fakeConn{}}
	sender := NewSenderWithDialer(dialer)

	sent, err := sender.Send(context.Background(), &Target{
		MAC:       mustMAC(t, "aa:bb:cc:dd:ee:ff"),
		Broadcast: "255.255.255.255",
		Port:      9,
		Count:     0,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sent != 1 {
		t.Fatalf("sent = %d, want 1", sent)
	}
}

func TestSendReportsPartialProgress(t *testing.T) {
	dialer := &fakeDialer{conn: &fakeConn{err: errors.New("network unreachable")}}
	sender := NewSenderWithDialer(dialer)

	sent, err := sender.Send(context.Background(), &Target{
		MAC:       mustMAC(t, "aa:bb:cc:dd:ee:ff"),
		Broadcast: "255.255.255.255",
		Port:      9,
		Count:     3,
	})
	if err == nil {
		t.Fatal("Send must report the write error")
	}
	if sent != 0 {
		t.Fatalf("sent = %d, want 0", sent)
	}
}

func TestSendStopsWhenContextIsCancelled(t *testing.T) {
	dialer := &fakeDialer{conn: &fakeConn{}}
	sender := NewSenderWithDialer(dialer)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sent, err := sender.Send(ctx, &Target{
		MAC:       mustMAC(t, "aa:bb:cc:dd:ee:ff"),
		Broadcast: "255.255.255.255",
		Port:      9,
		Count:     4,
		Interval:  time.Second,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// The first packet goes out before the interval is ever waited on, which
	// is what makes a cancelled wake still worth reporting.
	if sent != 1 {
		t.Fatalf("sent = %d, want 1", sent)
	}
}

func TestSendOverLoopback(t *testing.T) {
	var listener net.ListenConfig
	conn, err := listener.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = conn.Close() }()

	sender, err := NewSender("")
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	host, port, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	target := &Target{MAC: mustMAC(t, "aa:bb:cc:dd:ee:ff"), Broadcast: host, Count: 1}
	target.Port = atoi(t, port)

	if _, err := sender.Send(context.Background(), target); err != nil {
		t.Fatalf("Send: %v", err)
	}

	buf := make([]byte, 1024)
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	n, _, err := conn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != PacketLen {
		t.Fatalf("received %d bytes, want %d", n, PacketLen)
	}
}

func TestNewSenderRejectsUnresolvableSource(t *testing.T) {
	if _, err := NewSender("this is not an address"); err == nil {
		t.Fatal("an unresolvable source address must be rejected at construction")
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("port %q is not numeric", s)
		}
		n = n*10 + int(c-'0')
	}
	return n
}
