//go:build linux

package vip

import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"
)

// SendGratuitousARP sends a gratuitous ARP announcement for the given IP
// on the specified network interface to update network switch MAC tables.
func SendGratuitousARP(ifaceName string, ip net.IP) error {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return fmt.Errorf("getting interface %s: %w", ifaceName, err)
	}

	ip4 := ip.To4()
	if ip4 == nil {
		return fmt.Errorf("only IPv4 is supported for gratuitous ARP")
	}

	// Build ARP packet.
	// Ethernet header (14 bytes) + ARP payload (28 bytes) = 42 bytes
	packet := make([]byte, 42)

	// Ethernet header
	// Destination: broadcast (ff:ff:ff:ff:ff:ff)
	copy(packet[0:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	// Source: our MAC
	copy(packet[6:12], iface.HardwareAddr)
	// EtherType: ARP (0x0806)
	binary.BigEndian.PutUint16(packet[12:14], 0x0806)

	// ARP payload
	binary.BigEndian.PutUint16(packet[14:16], 1)    // Hardware type: Ethernet
	binary.BigEndian.PutUint16(packet[16:18], 0x0800) // Protocol type: IPv4
	packet[18] = 6                                      // Hardware address length
	packet[19] = 4                                      // Protocol address length
	binary.BigEndian.PutUint16(packet[20:22], 1)       // Operation: ARP Request (gratuitous)

	// Sender hardware address
	copy(packet[22:28], iface.HardwareAddr)
	// Sender protocol address
	copy(packet[28:32], ip4)
	// Target hardware address (broadcast for gratuitous)
	copy(packet[32:38], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	// Target protocol address (same as sender for gratuitous)
	copy(packet[38:42], ip4)

	// Open raw socket.
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(htons(syscall.ETH_P_ARP)))
	if err != nil {
		return fmt.Errorf("creating raw socket: %w", err)
	}
	defer syscall.Close(fd)

	// Build sockaddr.
	sa := &syscall.SockaddrLinklayer{
		Protocol: htons(syscall.ETH_P_ARP),
		Ifindex:  iface.Index,
		Halen:    6,
	}
	copy(sa.Addr[:], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})

	if err := syscall.Sendto(fd, packet, 0, sa); err != nil {
		return fmt.Errorf("sending ARP packet: %w", err)
	}

	return nil
}

func htons(v uint16) uint16 {
	return (v << 8) | (v >> 8)
}
