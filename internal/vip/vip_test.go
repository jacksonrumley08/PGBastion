package vip

import (
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
)

// These tests require CAP_NET_ADMIN and CAP_NET_RAW.
// Run with: sudo go test ./internal/vip/ -v -count=1

const (
	testIface = "lo"
	testVIP   = "192.0.2.99" // RFC 5737 TEST-NET, safe to assign to lo
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func skipIfNotRoot(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("skipping: requires root/CAP_NET_ADMIN (run with sudo)")
	}
}

func hasAddr(t *testing.T, ifaceName, ip string) bool {
	t.Helper()
	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		t.Fatalf("getting interface %s: %v", ifaceName, err)
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		t.Fatalf("listing addresses: %v", err)
	}
	for _, a := range addrs {
		if a.IP.String() == ip {
			return true
		}
	}
	return false
}

func removeTestVIP(t *testing.T) {
	t.Helper()
	link, err := netlink.LinkByName(testIface)
	if err != nil {
		return
	}
	addr, _ := netlink.ParseAddr(testVIP + "/32")
	netlink.AddrDel(link, addr)
}

func TestAssignVIP(t *testing.T) {
	skipIfNotRoot(t)
	removeTestVIP(t)
	defer removeTestVIP(t)

	m := NewManager(testVIP, testIface, "test-node", 3e9, nil, testLogger())

	err := m.assignVIP()
	if err != nil {
		t.Fatalf("assignVIP failed: %v", err)
	}

	if !hasAddr(t, testIface, testVIP) {
		t.Errorf("VIP %s not found on %s after assignVIP", testVIP, testIface)
	}

	// assignVIP doesn't set isOwner — that's done by campaignForVIP.
	// Verify it's still false, then set it manually to test the full flow.
	if m.IsOwner() {
		t.Error("expected isOwner=false after assignVIP (set by campaign, not assign)")
	}
	m.isOwner.Store(true)
	if !m.IsOwner() {
		t.Error("expected isOwner=true after manual store")
	}
}

func TestRemoveVIP(t *testing.T) {
	skipIfNotRoot(t)
	removeTestVIP(t)
	defer removeTestVIP(t)

	m := NewManager(testVIP, testIface, "test-node", 3e9, nil, testLogger())

	// Assign first.
	if err := m.assignVIP(); err != nil {
		t.Fatalf("assignVIP failed: %v", err)
	}
	m.isOwner.Store(true)

	// Now remove.
	m.removeVIP()

	if hasAddr(t, testIface, testVIP) {
		t.Errorf("VIP %s still present on %s after removeVIP", testVIP, testIface)
	}
	if m.IsOwner() {
		t.Error("expected isOwner=false after removeVIP")
	}
}

func TestAssignVIP_AlreadyExists(t *testing.T) {
	skipIfNotRoot(t)
	removeTestVIP(t)
	defer removeTestVIP(t)

	m := NewManager(testVIP, testIface, "test-node", 3e9, nil, testLogger())

	// Assign twice — second should not error.
	if err := m.assignVIP(); err != nil {
		t.Fatalf("first assignVIP failed: %v", err)
	}
	if err := m.assignVIP(); err != nil {
		t.Fatalf("second assignVIP (already exists) should not error: %v", err)
	}

	if !hasAddr(t, testIface, testVIP) {
		t.Error("VIP should still be assigned")
	}
}

func TestRemoveVIP_NotOwner(t *testing.T) {
	skipIfNotRoot(t)

	m := NewManager(testVIP, testIface, "test-node", 3e9, nil, testLogger())
	// isOwner is false by default — removeVIP should be a no-op and not panic.
	m.removeVIP()
}

func TestAssignVIP_BadInterface(t *testing.T) {
	skipIfNotRoot(t)

	m := NewManager(testVIP, "nonexistent0", "test-node", 3e9, nil, testLogger())
	err := m.assignVIP()
	if err == nil {
		t.Error("expected error for nonexistent interface")
	}
}

func TestAssignVIP_VerifyWithIPCommand(t *testing.T) {
	skipIfNotRoot(t)
	removeTestVIP(t)
	defer removeTestVIP(t)

	m := NewManager(testVIP, testIface, "test-node", 3e9, nil, testLogger())
	if err := m.assignVIP(); err != nil {
		t.Fatalf("assignVIP failed: %v", err)
	}

	// Cross-check with the `ip` command.
	out, err := exec.Command("ip", "addr", "show", testIface).CombinedOutput()
	if err != nil {
		t.Fatalf("ip addr show failed: %v", err)
	}
	if !strings.Contains(string(out), testVIP) {
		t.Errorf("ip addr show %s does not contain %s:\n%s", testIface, testVIP, out)
	}
}

func TestGratuitousARP_OnLoopback(t *testing.T) {
	skipIfNotRoot(t)

	// Loopback doesn't actually send ARP on the wire, but this validates
	// the packet construction and syscall don't error.
	err := SendGratuitousARP(testIface, net.ParseIP(testVIP))
	if err != nil {
		t.Fatalf("SendGratuitousARP failed: %v", err)
	}
}

func TestGratuitousARP_BadInterface(t *testing.T) {
	skipIfNotRoot(t)

	err := SendGratuitousARP("nonexistent0", net.ParseIP(testVIP))
	if err == nil {
		t.Error("expected error for nonexistent interface")
	}
}

func TestGratuitousARP_IPv6Rejected(t *testing.T) {
	skipIfNotRoot(t)

	err := SendGratuitousARP(testIface, net.ParseIP("::1"))
	if err == nil {
		t.Error("expected error for IPv6 address")
	}
}
