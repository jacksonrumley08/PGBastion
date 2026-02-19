//go:build !linux

package vip

import (
	"fmt"
	"net"
)

// SendGratuitousARP is not supported on non-Linux platforms.
func SendGratuitousARP(ifaceName string, ip net.IP) error {
	return fmt.Errorf("gratuitous ARP is not supported on this platform")
}
