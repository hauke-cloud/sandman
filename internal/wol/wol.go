// Package wol builds and sends Wake-on-LAN magic packets.
//
// A magic packet is not a protocol so much as a pattern: six 0xff bytes
// followed by the target's MAC address repeated sixteen times. A sleeping NIC
// scans every frame it receives for that pattern and wakes the machine when it
// matches, which is why the packet works over UDP to any port, and why nothing
// ever answers it. There is no acknowledgement, so a successful Send means the
// bytes left this host, nothing more.
package wol

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	// macLen is the length of an Ethernet hardware address.
	macLen = 6
	// repeats is how many times the MAC is repeated in a magic packet.
	repeats = 16
	// passwordLen is the length of a SecureOn password, when one is used.
	passwordLen = 6
	// PacketLen is the size of a magic packet without a SecureOn password.
	PacketLen = macLen + repeats*macLen
)

// ErrInvalidPassword is returned for a SecureOn password that is not six bytes.
var ErrInvalidPassword = errors.New("secureOn password must be 6 bytes")

// BuildPacket returns the magic packet for mac, optionally carrying a SecureOn
// password. password may be nil.
func BuildPacket(mac net.HardwareAddr, password []byte) ([]byte, error) {
	if len(mac) != macLen {
		return nil, fmt.Errorf("mac address must be %d bytes, got %d", macLen, len(mac))
	}
	if password != nil && len(password) != passwordLen {
		return nil, ErrInvalidPassword
	}

	packet := make([]byte, 0, PacketLen+len(password))
	packet = append(packet, bytes.Repeat([]byte{0xff}, macLen)...)
	for range repeats {
		packet = append(packet, mac...)
	}
	return append(packet, password...), nil
}

// ParseMAC accepts the colon- and dash-separated forms a SandmanDevice allows.
func ParseMAC(s string) (net.HardwareAddr, error) {
	mac, err := net.ParseMAC(strings.ReplaceAll(strings.TrimSpace(s), "-", ":"))
	if err != nil {
		return nil, fmt.Errorf("parse mac address %q: %w", s, err)
	}
	if len(mac) != macLen {
		return nil, fmt.Errorf("mac address %q is not a 6-byte Ethernet address", s)
	}
	return mac, nil
}

// ParsePassword reads a SecureOn password, which may be written as six raw
// bytes or as hex with any of the usual separators. Both forms turn up in the
// wild, and a Secret written either way should work.
//
// Hex wins where a value could be read either way, except when reading it as
// hex would not produce six bytes -- so "abcdef" is the six characters, not the
// three bytes 0xab 0xcd 0xef, because three bytes is not a password.
func ParsePassword(raw []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}

	cleaned := strings.NewReplacer(":", "", "-", "", ".", "", " ", "").Replace(string(trimmed))
	if decoded, err := hex.DecodeString(cleaned); err == nil && len(decoded) == passwordLen {
		return decoded, nil
	}
	if len(trimmed) == passwordLen {
		return trimmed, nil
	}
	return nil, ErrInvalidPassword
}

// Target describes one machine to wake.
type Target struct {
	// MAC is the hardware address the packet names.
	MAC net.HardwareAddr
	// Broadcast is the address the packet is sent to, usually a broadcast
	// address.
	Broadcast string
	// Port is the UDP port. Nothing listens on it.
	Port int
	// Count is how many identical packets to send.
	Count int
	// Interval is the gap between them.
	Interval time.Duration
	// Password is an optional six-byte SecureOn password.
	Password []byte
}

// Dialer opens the UDP socket a packet is written to. It exists so tests can
// listen on loopback instead of broadcasting onto a real network.
type Dialer interface {
	Dial(ctx context.Context, address string) (net.Conn, error)
}

// Sender sends magic packets.
type Sender struct {
	dialer Dialer
}

// NewSender returns a Sender that broadcasts from the given local address.
// An empty source lets the kernel choose, which is right on a host with one
// network. On a host with several, the packet has to leave the interface that
// faces the machines being woken, and only an explicit source address makes
// that choice.
//
// The Go runtime sets SO_BROADCAST on datagram sockets, so no extra socket
// option is needed to reach a broadcast address.
func NewSender(source string) (*Sender, error) {
	dialer := &net.Dialer{}
	if source != "" {
		addr, err := resolveSource(source)
		if err != nil {
			return nil, err
		}
		dialer.LocalAddr = addr
	}
	return &Sender{dialer: netDialer{dialer}}, nil
}

// NewSenderWithDialer returns a Sender using a caller-supplied dialer.
func NewSenderWithDialer(d Dialer) *Sender { return &Sender{dialer: d} }

// Send writes the target's magic packets. It returns the number written, which
// is what a caller reports when only some of them made it out.
func (s *Sender) Send(ctx context.Context, target *Target) (int, error) {
	packet, err := BuildPacket(target.MAC, target.Password)
	if err != nil {
		return 0, err
	}

	address := net.JoinHostPort(target.Broadcast, strconv.Itoa(target.Port))
	conn, err := s.dialer.Dial(ctx, address)
	if err != nil {
		return 0, fmt.Errorf("dial %s: %w", address, err)
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(deadline)
	}

	count := max(target.Count, 1)
	sent := 0
	for i := range count {
		if i > 0 && target.Interval > 0 {
			// A NIC that missed the first packet is often still settling, so
			// the gap between repeats matters more than the repeats do.
			select {
			case <-ctx.Done():
				return sent, ctx.Err()
			case <-time.After(target.Interval):
			}
		}
		if _, err := conn.Write(packet); err != nil {
			return sent, fmt.Errorf("send magic packet to %s: %w", address, err)
		}
		sent++
	}
	return sent, nil
}

func resolveSource(source string) (*net.UDPAddr, error) {
	// A bare address is the common form; a host:port lets an operator pin the
	// source port too, which some firewalls are written against.
	if _, _, err := net.SplitHostPort(source); err != nil {
		source = net.JoinHostPort(source, "0")
	}
	addr, err := net.ResolveUDPAddr("udp", source)
	if err != nil {
		return nil, fmt.Errorf("resolve source address %q: %w", source, err)
	}
	return addr, nil
}

type netDialer struct{ *net.Dialer }

func (d netDialer) Dial(ctx context.Context, address string) (net.Conn, error) {
	return d.DialContext(ctx, "udp", address)
}
