// wgtcp — WireGuard peer in pure userspace, transport over TCP.
//
// This VM cannot use UDP at all (sendto/recvfrom return EPERM) and cannot
// create TUN devices outside a user namespace, so a normal WireGuard client
// cannot run here. Instead this program:
//   - runs wireguard-go with a custom conn.Bind that carries WG packets
//     over a TCP connection (4-byte big-endian length framing) to a relay
//     on Cloud D, which converts them back to UDP for the real wg0 server;
//   - uses gVisor netstack as the TUN device, so no kernel interface is
//     needed at all;
//   - forwards local TCP ports through the tunnel (ADB to the phone, plus
//     the WG panel for testing).
//
// The private key is read from the wg config file and never logged.
package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

const maxFrame = 65535

// ---------------------------------------------------------------------------
// Config file parsing (standard wg .conf format, base64 keys).
// ---------------------------------------------------------------------------

type wgConfig struct {
	privateKeyB64 string
	address       netip.Addr
	mtu           int
	peerKeyB64    string
	allowedIPs    []string
	keepalive     int
}

func parseConfig(path string) (*wgConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &wgConfig{mtu: 1420}
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = strings.Trim(line, "[]")
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch section {
		case "Interface":
			switch k {
			case "PrivateKey":
				cfg.privateKeyB64 = v
			case "Address":
				addr, err := netip.ParsePrefix(v)
				if err != nil {
					return nil, fmt.Errorf("bad Address %q: %w", v, err)
				}
				cfg.address = addr.Addr()
			case "MTU":
				fmt.Sscanf(v, "%d", &cfg.mtu)
			}
		case "Peer":
			switch k {
			case "PublicKey":
				cfg.peerKeyB64 = v
			case "AllowedIPs":
				for _, ip := range strings.Split(v, ",") {
					cfg.allowedIPs = append(cfg.allowedIPs, strings.TrimSpace(ip))
				}
			case "PersistentKeepalive":
				fmt.Sscanf(v, "%d", &cfg.keepalive)
			}
		}
	}
	if cfg.privateKeyB64 == "" || cfg.peerKeyB64 == "" || !cfg.address.IsValid() {
		return nil, fmt.Errorf("config %s is missing PrivateKey, PublicKey or Address", path)
	}
	return cfg, nil
}

func b64ToHex(s string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("key is %d bytes, want 32", len(raw))
	}
	return hex.EncodeToString(raw), nil
}

// ---------------------------------------------------------------------------
// TCP transport for WireGuard packets.
// ---------------------------------------------------------------------------

type tcpEndpoint struct{}

func (tcpEndpoint) ClearSrc()           {}
func (tcpEndpoint) SrcToString() string { return "tcp:local" }
func (tcpEndpoint) DstToString() string { return "tcp:relay" }
func (tcpEndpoint) DstToBytes() []byte  { return []byte{127, 0, 0, 1} }
func (tcpEndpoint) DstIP() netip.Addr   { return netip.AddrFrom4([4]byte{127, 0, 0, 1}) }
func (tcpEndpoint) SrcIP() netip.Addr   { return netip.AddrFrom4([4]byte{127, 0, 0, 1}) }

type tcpBind struct {
	mu      sync.Mutex
	conn    net.Conn
	addr    string
	packets chan []byte
	stop    chan struct{}
	wg      sync.WaitGroup
}

func newTCPBind(addr string) *tcpBind {
	return &tcpBind{
		addr:    addr,
		packets: make(chan []byte, 128),
		stop:    make(chan struct{}),
	}
}

func (b *tcpBind) dial() (net.Conn, error) {
	var d net.Dialer
	d.Timeout = 5 * time.Second
	return d.Dial("tcp", b.addr)
}

func (b *tcpBind) reader(stop chan struct{}) {
	defer b.wg.Done()
	backoff := time.Second
	for {
		select {
		case <-stop:
			return
		default:
		}
		c, err := b.dial()
		if err != nil {
			log.Printf("tcpBind: dial %s failed: %v (retry in %v)", b.addr, err, backoff)
			select {
			case <-stop:
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		b.mu.Lock()
		b.conn = c
		b.mu.Unlock()
		log.Printf("tcpBind: connected to %s", b.addr)
		for {
			hdr := make([]byte, 4)
			if _, err := io.ReadFull(c, hdr); err != nil {
				break
			}
			ln := binary.BigEndian.Uint32(hdr)
			if ln == 0 || ln > maxFrame {
				log.Printf("tcpBind: bad frame length %d", ln)
				break
			}
			buf := make([]byte, ln)
			if _, err := io.ReadFull(c, buf); err != nil {
				break
			}
			select {
			case b.packets <- buf:
			case <-stop:
				c.Close()
				return
			}
		}
		b.mu.Lock()
		if b.conn == c {
			b.conn = nil
		}
		b.mu.Unlock()
		c.Close()
		log.Printf("tcpBind: connection lost, reconnecting")
	}
}

func (b *tcpBind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	// The device may Close() the bind before it is ever Open()ed (during
	// IpcSet) and may Open() it again after a Close() (BindUpdate). Each
	// generation therefore gets a fresh stop channel; closing the old one
	// must not poison the next generation.
	b.mu.Lock()
	if b.stop == nil {
		b.stop = make(chan struct{})
	}
	stop := b.stop
	b.mu.Unlock()
	b.wg.Add(1)
	go b.reader(stop)
	fn := func(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
		select {
		case pkt := <-b.packets:
			copy(packets[0], pkt)
			sizes[0] = len(pkt)
			eps[0] = tcpEndpoint{}
			return 1, nil
		case <-stop:
			return 0, net.ErrClosed
		}
	}
	return []conn.ReceiveFunc{fn}, 0, nil
}

func (b *tcpBind) Close() error {
	b.mu.Lock()
	if b.stop != nil {
		close(b.stop)
		b.stop = nil
	}
	if b.conn != nil {
		b.conn.Close()
		b.conn = nil
	}
	b.mu.Unlock()
	return nil
}

func (b *tcpBind) SetMark(mark uint32) error { return nil }

func (b *tcpBind) Send(bufs [][]byte, ep conn.Endpoint) error {
	b.mu.Lock()
	c := b.conn
	b.mu.Unlock()
	if c == nil {
		return fmt.Errorf("tcpBind: not connected")
	}
	for _, buf := range bufs {
		frame := make([]byte, 4+len(buf))
		binary.BigEndian.PutUint32(frame, uint32(len(buf)))
		copy(frame[4:], buf)
		c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Write(frame); err != nil {
			b.mu.Lock()
			if b.conn == c {
				b.conn = nil
			}
			b.mu.Unlock()
			c.Close()
			return err
		}
	}
	return nil
}

func (b *tcpBind) ParseEndpoint(s string) (conn.Endpoint, error) {
	return tcpEndpoint{}, nil
}

func (b *tcpBind) BatchSize() int { return 1 }

// ---------------------------------------------------------------------------
// TCP forwarding through the tunnel.
// ---------------------------------------------------------------------------

func forwardTCP(listenAddr string, tnet *netstack.Net, target netip.AddrPort) {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("listen %s: %v", listenAddr, err)
	}
	log.Printf("forward %s -> %s", listenAddr, target)
	for {
		client, err := ln.Accept()
		if err != nil {
			log.Printf("accept %s: %v", listenAddr, err)
			continue
		}
		go func(client net.Conn) {
			defer client.Close()
			remote, err := tnet.DialTCPAddrPort(target)
			if err != nil {
				log.Printf("dial %s via tunnel: %v", target, err)
				return
			}
			defer remote.Close()
			done := make(chan struct{}, 2)
			go func() {
				io.Copy(remote, client)
				done <- struct{}{}
			}()
			go func() {
				io.Copy(client, remote)
				done <- struct{}{}
			}()
			<-done
		}(client)
	}
}

// ---------------------------------------------------------------------------
// Main.
// ---------------------------------------------------------------------------

func main() {
	configPath := flag.String("config", "", "WireGuard .conf file (default ~/workspace/wg-muse-a.conf)")
	relayAddr := flag.String("relay", "127.0.0.1:51822", "TCP address of the WG packet relay")
	flag.Parse()
	if *configPath == "" {
		home, _ := os.UserHomeDir()
		*configPath = home + "/workspace/wg-muse-a.conf"
	}

	cfg, err := parseConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	privHex, err := b64ToHex(cfg.privateKeyB64)
	if err != nil {
		log.Fatalf("private key: %v", err)
	}
	peerHex, err := b64ToHex(cfg.peerKeyB64)
	if err != nil {
		log.Fatalf("peer key: %v", err)
	}

	tunDev, tnet, err := netstack.CreateNetTUN(
		[]netip.Addr{cfg.address}, nil, cfg.mtu)
	if err != nil {
		log.Fatalf("CreateNetTUN: %v", err)
	}
	log.Printf("tunnel interface up as %s (mtu %d)", cfg.address, cfg.mtu)

	bind := newTCPBind(*relayAddr)
	logger := device.NewLogger(device.LogLevelVerbose, "wg: ")
	dev := device.NewDevice(tunDev, bind, logger)

	var uapi strings.Builder
	fmt.Fprintf(&uapi, "private_key=%s\n", privHex)
	fmt.Fprintf(&uapi, "public_key=%s\n", peerHex)
	fmt.Fprintf(&uapi, "endpoint=%s\n", *relayAddr)
	for _, ip := range cfg.allowedIPs {
		fmt.Fprintf(&uapi, "allowed_ip=%s\n", ip)
	}
	if cfg.keepalive > 0 {
		fmt.Fprintf(&uapi, "persistent_keepalive_interval=%d\n", cfg.keepalive)
	}
	if err := dev.IpcSet(uapi.String()); err != nil {
		log.Fatalf("IpcSet: %v", err)
	}
	if err := dev.Up(); err != nil {
		log.Fatalf("device up: %v", err)
	}
	log.Printf("wireguard device up, peer via tcp relay %s", *relayAddr)

	// Local forwards through the tunnel.
	go forwardTCP("127.0.0.1:15555", tnet, netip.MustParseAddrPort("10.200.200.3:5555")) // ADB (phone)
	go forwardTCP("127.0.0.1:18088", tnet, netip.MustParseAddrPort("10.200.200.1:8088")) // WG panel

	// Status endpoint (private key stripped).
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		out, err := dev.IpcGet()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		var lines []string
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "private_key=") || strings.HasPrefix(line, "preshared_key=") {
				continue
			}
			lines = append(lines, line)
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, strings.Join(lines, "\n"))
	})
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})
	go func() {
		log.Printf("status on http://127.0.0.1:18080/status")
		if err := http.ListenAndServe("127.0.0.1:18080", nil); err != nil {
			log.Printf("status server: %v", err)
		}
	}()

	// Wait for a termination signal.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down")
	dev.Close()
	_ = context.Background()
}
