package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Konfigurasi & path
// ---------------------------------------------------------------------------

type Config struct {
	Prefix string // $PREFIX (Termux) atau ~/.torctl
	RunDir string // pid file + torrc yang digenerate
	LogDir string // tor.log, bridge.log, snowflake.log
	LibDir string // DataDirectory tor (bertahan antar restart)

	TorBin        string
	SnowflakeBin  string
	Snowflake     bool
	Bridges       []string
	SnowflakeURL  string
	SnowflakeList string // daftar front domain (dipisah koma), opsional
	SnowflakeICE  string

	SocksAddr  string // SOCKS5 tor
	BridgeAddr string // HTTP bridge (HTTP ↔ SOCKS5)

	BootstrapTimeout time.Duration
	CheckURL         string
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// loadConfig membaca konfigurasi dari environment.
//
//	PREFIX / TORCTL_PREFIX  → layout $PREFIX/var/{run,log,lib}/torctl
//	tanpa prefix             → layout ~/.torctl/{run,log,lib} (mode home)
func loadConfig() *Config {
	c := &Config{}

	prefix := os.Getenv("TORCTL_PREFIX")
	if prefix == "" {
		prefix = os.Getenv("PREFIX")
	}
	homeMode := false
	if prefix == "" {
		if _, err := os.Stat("/data/data/com.termux/files/usr"); err == nil {
			prefix = "/data/data/com.termux/files/usr"
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				home = "."
			}
			prefix = filepath.Join(home, ".torctl")
			homeMode = true
		}
	}
	c.Prefix = prefix

	if homeMode {
		c.RunDir = filepath.Join(prefix, "run")
		c.LogDir = filepath.Join(prefix, "log")
		c.LibDir = filepath.Join(prefix, "lib")
	} else {
		c.RunDir = filepath.Join(prefix, "var", "run", "torctl")
		c.LogDir = filepath.Join(prefix, "var", "log", "torctl")
		c.LibDir = filepath.Join(prefix, "var", "lib", "torctl")
	}

	c.TorBin = getenv("TORCTL_TOR_BIN", "tor")
	c.SnowflakeBin = getenv("TORCTL_SNOWFLAKE_BIN", "snowflake-client")
	c.Snowflake = true
	switch strings.ToLower(os.Getenv("TORCTL_SNOWFLAKE")) {
	case "0", "false", "off", "no":
		c.Snowflake = false
	}
	c.SocksAddr = getenv("TORCTL_SOCKS_ADDR", "127.0.0.1:9050")
	c.BridgeAddr = getenv("TORCTL_BRIDGE_ADDR", "127.0.0.1:8118")

	c.SnowflakeURL = getenv("TORCTL_SNOWFLAKE_URL",
		"https://snowflake-broker.torproject.net.global.prod.fastly.net/")
	c.SnowflakeList = os.Getenv("TORCTL_SNOWFLAKE_FRONTS") // opsional
	c.SnowflakeICE = getenv("TORCTL_SNOWFLAKE_ICE",
		"stun:stun.l.google.com:19302,"+
			"stun:stun.voip.blackberry.com:3478,"+
			"stun:stun.uls.co.za:3478,"+
			"stun:stun.stunprotocol.org:3478")

	c.CheckURL = getenv("TORCTL_CHECK_URL", "https://check.torproject.org/api/ip")
	c.BootstrapTimeout = 180 * time.Second
	if v := os.Getenv("TORCTL_BOOTSTRAP_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.BootstrapTimeout = d
		}
	}

	// Bridge custom: dipisah ";" atau newline. Prefiks "Bridge " opsional.
	for _, part := range strings.FieldsFunc(os.Getenv("TORCTL_BRIDGES"),
		func(r rune) bool { return r == ';' || r == '\n' }) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		part = strings.TrimPrefix(part, "Bridge ")
		c.Bridges = append(c.Bridges, part)
	}
	return c
}

func (c *Config) torctlPIDPath() string { return filepath.Join(c.RunDir, "torctl.pid") }
func (c *Config) torPIDPath() string    { return filepath.Join(c.RunDir, "tor.pid") }
func (c *Config) sfPIDPath() string     { return filepath.Join(c.RunDir, "snowflake.pid") }
func (c *Config) torrcPath() string     { return filepath.Join(c.RunDir, "torctl.torrc") }
func (c *Config) torLogPath() string    { return filepath.Join(c.LogDir, "tor.log") }
func (c *Config) bridgeLogPath() string { return filepath.Join(c.LogDir, "bridge.log") }
func (c *Config) sfLogPath() string     { return filepath.Join(c.LogDir, "snowflake.log") }
func (c *Config) dataDir() string       { return filepath.Join(c.LibDir, "tor-data") }

func (c *Config) ensureDirs() error {
	for _, d := range []string{c.RunDir, c.LogDir, c.LibDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// PID file
// ---------------------------------------------------------------------------

func readPID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func writePID(path string, pid int) {
	os.WriteFile(path, []byte(strconv.Itoa(pid)), 0644)
}

func removePID(path string) { os.Remove(path) }

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// procCmdline mengembalikan cmdline proses (argv, dipisah spasi).
func procCmdline(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(string(b), "\x00", " ")
}

// pidMatches memastikan pid masih menjalankan binary yang diharapkan
// (melindungi dari pid reuse setelah crash).
func pidMatches(pid int, bin string) bool {
	want := filepath.Base(bin)
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return false
	}
	argv0 := strings.SplitN(strings.ReplaceAll(string(b), "\x00", " "), " ", 2)[0]
	return filepath.Base(argv0) == want
}

// procPgid mengembalikan process group id proses; 0 bila tak dikenal.
func procPgid(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	s := string(b)
	i := strings.LastIndex(s, ")")
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:]) // state ppid pgrp ...
	if len(f) < 3 {
		return 0
	}
	n, _ := strconv.Atoi(f[2])
	return n
}

// killPID mengirim sinyal ke proses; group=true membunuh seluruh process
// group-nya (aman hanya bila pgid == pid, yaitu anak yang di-Setpgid).
func killPID(pid int, sig syscall.Signal, group bool) {
	if pid <= 0 {
		return
	}
	if group && procPgid(pid) == pid {
		syscall.Kill(-pid, sig)
	}
	syscall.Kill(pid, sig)
}

// stopPIDFile menghentikan proses berdasar pid file: SIGTERM → tunggu →
// SIGKILL, lalu hapus pid file. match dipakai untuk validasi pid reuse.
func stopPIDFile(path, match string, group bool, grace time.Duration) {
	pid := readPID(path)
	if pid <= 0 {
		return
	}
	if !pidAlive(pid) {
		removePID(path)
		return
	}
	if match != "" && !pidMatches(pid, match) {
		fmt.Fprintf(os.Stderr,
			"torctl: %s pid %d kini milik proses lain (%s), dilewati\n",
			filepath.Base(path), pid, procCmdline(pid))
		removePID(path)
		return
	}
	killPID(pid, syscall.SIGTERM, group)
	deadline := time.Now().Add(grace)
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if pidAlive(pid) {
		killPID(pid, syscall.SIGKILL, group)
		time.Sleep(200 * time.Millisecond)
	}
	removePID(path)
}

// ---------------------------------------------------------------------------
// Log helpers
// ---------------------------------------------------------------------------

// tailFile mencetak n baris terakhir dari file.
func tailFile(path string, n int) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Println("(no log)")
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	start := len(lines) - n
	if start < 0 {
		start = 0
	}
	for _, l := range lines[start:] {
		fmt.Println(l)
	}
}

type fileTail struct {
	name    string
	path    string
	off     int64
	pending []byte
}

// readTailLines mengembalikan baris baru yang muncul sejak terakhir baca.
func (t *fileTail) readTailLines() ([]string, error) {
	st, err := os.Stat(t.path)
	if err != nil {
		return nil, err
	}
	if st.Size() < t.off { // file di-truncate (restart)
		t.off = 0
		t.pending = t.pending[:0]
	}
	if st.Size() == t.off {
		return nil, nil
	}
	f, err := os.Open(t.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(t.off, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, st.Size()-t.off)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	t.off = st.Size()
	t.pending = append(t.pending, buf...)
	var out []string
	for {
		i := indexByte(t.pending, '\n')
		if i < 0 {
			break
		}
		out = append(out, string(t.pending[:i]))
		t.pending = t.pending[i+1:]
	}
	return out, nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// Probe & status helpers
// ---------------------------------------------------------------------------

func portListening(addr string) bool {
	d := net.Dialer{Timeout: 700 * time.Millisecond}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// checkTorViaSOCKS menembak checkURL lewat SOCKS5 tor, mengembalikan body
// (mis. {"IsTor":true,"IP":"..."}).
func checkTorViaSOCKS(socksAddr, checkURL string) (string, error) {
	d := &socks5Dialer{addr: socksAddr, timeout: 10 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return d.Dial(network, addr)
		},
	}
	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(ctx, "GET", checkURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// pidLine format baris status: "1234 (alive=true)" / "0 (alive=false)".
func pidLine(pid int) string {
	return strconv.Itoa(pid) + " (alive=" + strconv.FormatBool(pidAlive(pid)) + ")"
}
