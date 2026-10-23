package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// HTTP ↔ SOCKS5 bridge (berjalan di dalam proses torctl)
//
//   HTTP CONNECT / plain-HTTP  →  dial via SOCKS5 tor  →  target
//
// Dipakai: curl -x http://127.0.0.1:8118 https://...
// ---------------------------------------------------------------------------

type bridge struct {
	ln     net.Listener
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

var brg *bridge

func startBridge(c *Config) error {
	dialer := &socks5Dialer{addr: c.SocksAddr, timeout: 15 * time.Second}
	ln, err := net.Listen("tcp", c.BridgeAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", c.BridgeAddr, err)
	}
	brg = &bridge{ln: ln, conns: make(map[net.Conn]struct{})}
	bridgeLogPathGlobal = c.bridgeLogPath()
	go brg.acceptLoop(dialer)
	return nil
}

func stopBridge() {
	if brg == nil {
		return
	}
	brg.mu.Lock()
	brg.closed = true
	conns := make([]net.Conn, 0, len(brg.conns))
	for cn := range brg.conns {
		conns = append(conns, cn)
	}
	brg.mu.Unlock()

	brg.ln.Close()
	// paksa tutup koneksi yang masih aktif (deadline di masa lalu)
	past := time.Now().Add(-time.Second)
	for _, cn := range conns {
		cn.SetDeadline(past)
	}
}

func (b *bridge) acceptLoop(dialer *socks5Dialer) {
	for {
		c, err := b.ln.Accept()
		if err != nil {
			b.mu.Lock()
			closed := b.closed
			b.mu.Unlock()
			if !closed {
				blf("accept error: %v", err)
			}
			return
		}
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			c.Close()
			return
		}
		b.conns[c] = struct{}{}
		b.mu.Unlock()
		go func() {
			defer func() {
				b.mu.Lock()
				delete(b.conns, c)
				b.mu.Unlock()
				c.Close()
			}()
			handleConn(c, dialer)
		}()
	}
}

func handleConn(c net.Conn, dialer *socks5Dialer) {
	// deadline hanya untuk handshake; dial dan tunnel dibebaskan sesudahnya
	c.SetDeadline(time.Now().Add(30 * time.Second))

	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return // bukan HTTP valid, diamkan
	}

	if req.Method == http.MethodConnect {
		handleConnect(c, br, req, dialer)
		return
	}
	handlePlain(c, br, req, dialer)
}

func handleConnect(c net.Conn, br *bufio.Reader, req *http.Request, dialer *socks5Dialer) {
	target, err := dialer.Dial("tcp", defaultPort(req.Host, "443"))
	if err != nil {
		blf("CONNECT %s gagal: %v", req.Host, err)
		fmt.Fprint(c, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")
		return
	}
	defer target.Close()

	c.SetDeadline(time.Time{})
	target.SetDeadline(time.Time{})
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
	}
	if tc, ok := target.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
	}

	if _, err := fmt.Fprint(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	blf("CONNECT %s ok", req.Host)

	done := make(chan struct{}, 2)
	go func() { io.Copy(target, br); done <- struct{}{} }()
	go func() { io.Copy(c, target); done <- struct{}{} }()
	<-done
	// tutup keduanya agar goroutine satunya ikut keluar
	c.Close()
	target.Close()
	<-done
}

func handlePlain(c net.Conn, br *bufio.Reader, req *http.Request, dialer *socks5Dialer) {
	// buang header hop-by-hop milik proxy
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Proxy-Authorization")

	target, err := dialer.Dial("tcp", defaultPort(req.Host, "80"))
	if err != nil {
		blf("HTTP %s gagal: %v", req.Host, err)
		fmt.Fprint(c, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")
		return
	}
	defer target.Close()

	c.SetDeadline(time.Time{})
	target.SetDeadline(time.Time{})

	if err := req.Write(target); err != nil {
		blf("HTTP %s tulis request gagal: %v", req.Host, err)
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(target), req)
	if err != nil {
		blf("HTTP %s baca response gagal: %v", req.Host, err)
		return
	}
	defer resp.Body.Close()
	if err := resp.Write(c); err != nil {
		blf("HTTP %s tulis response gagal: %v", req.Host, err)
		return
	}
	blf("HTTP %s ok", req.Host)
}

// defaultPort melengkapi alamat host:port bila Host header tanpa port
// (mis. "github.com" → "github.com:80").
func defaultPort(hostport, port string) string {
	if _, _, err := net.SplitHostPort(hostport); err == nil {
		return hostport
	}
	h := strings.TrimPrefix(strings.TrimSuffix(hostport, "]"), "[")
	return net.JoinHostPort(h, port)
}

// blf menulis satu baris log bridge ke bridge.log + stderr.
func blf(format string, args ...interface{}) {
	line := fmt.Sprintf(format, args...)
	log.Printf("[bridge] %s", line)
	appendLine(bridgeLogPathGlobal, line)
}

var (
	bridgeLogPathGlobal string
	bridgeLogMu         sync.Mutex
)

func appendLine(path, line string) {
	if path == "" {
		return
	}
	bridgeLogMu.Lock()
	defer bridgeLogMu.Unlock()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n",
		time.Now().Format("2006-01-02T15:04:05"), line)
}

// ---------------------------------------------------------------------------
// SOCKS5 dialer (klien mandiri, tanpa auth — cukup untuk tor)
// ---------------------------------------------------------------------------

type socks5Dialer struct {
	addr    string // alamat SOCKS5, mis. 127.0.0.1:9050
	timeout time.Duration
}

func (d *socks5Dialer) Dial(network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("socks5: jaringan %q tidak didukung", network)
	}
	conn, err := net.DialTimeout("tcp", d.addr, d.timeout)
	if err != nil {
		return nil, fmt.Errorf("socks5 dial %s: %w", d.addr, err)
	}
	if err := d.handshake(conn, address); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{}) // bebas untuk transfer
	return conn, nil
}

func (d *socks5Dialer) handshake(conn net.Conn, address string) error {
	conn.SetDeadline(time.Now().Add(d.timeout))

	// greeting: VER=5, NMETHODS=1, NO-AUTH
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return fmt.Errorf("socks5 greeting: %w", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("socks5 baca metode: %w", err)
	}
	if reply[0] != 0x05 || reply[1] != 0x00 {
		return fmt.Errorf("socks5 metode ditolak (ver=%d method=%d)", reply[0], reply[1])
	}

	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("socks5 alamat %q: %w", address, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("socks5 port %q: %w", portStr, err)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("socks5 port di luar rentang: %d", port)
	}

	// request CONNECT
	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			req = append(req, 0x01)
			req = append(req, v4...)
		} else {
			v6 := ip.To16()
			if v6 == nil {
				return fmt.Errorf("socks5 IP tidak valid: %s", host)
			}
			req = append(req, 0x04)
			req = append(req, v6...)
		}
	} else {
		if len(host) > 255 {
			return fmt.Errorf("socks5 nama host terlalu panjang")
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	var portBuf [2]byte
	binary.BigEndian.PutUint16(portBuf[:], uint16(port))
	req = append(req, portBuf[:]...)
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5 kirim request: %w", err)
	}

	// balasan: VER REP RSV ATYP + BND.ADDR + BND.PORT
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return fmt.Errorf("socks5 baca balasan: %w", err)
	}
	if hdr[0] != 0x05 {
		return fmt.Errorf("socks5 balasan ver=%d", hdr[0])
	}
	if hdr[1] != 0x00 {
		return fmt.Errorf("socks5 CONNECT ditolak: %s", socks5ReplyString(hdr[1]))
	}
	// buang BND.ADDR sesuai ATYP
	switch hdr[3] {
	case 0x01:
		if _, err := io.ReadFull(conn, make([]byte, 4)); err != nil {
			return err
		}
	case 0x04:
		if _, err := io.ReadFull(conn, make([]byte, 16)); err != nil {
			return err
		}
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return err
		}
		if _, err := io.ReadFull(conn, make([]byte, int(l[0]))); err != nil {
			return err
		}
	default:
		return fmt.Errorf("socks5 ATYP tak dikenal: %d", hdr[3])
	}
	if _, err := io.ReadFull(conn, make([]byte, 2)); err != nil {
		return err
	}
	return nil
}

func socks5ReplyString(rep byte) string {
	switch rep {
	case 0x01:
		return "general failure"
	case 0x02:
		return "connection not allowed"
	case 0x03:
		return "network unreachable"
	case 0x04:
		return "host unreachable"
	case 0x05:
		return "connection refused"
	case 0x06:
		return "TTL expired"
	case 0x07:
		return "command not supported"
	case 0x08:
		return "address type not supported"
	}
	return fmt.Sprintf("error %d", rep)
}
