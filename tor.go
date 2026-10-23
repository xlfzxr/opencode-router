package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Child process supervisor
//
// Setiap anak (tor, snowflake) di-start dari goroutine yang memegang
// runtime.LockOSThread sampai anak selesai, sehingga Pdeathsig (SIGKILL
// saat torctl mati) selalu aktif: torctl mati → anak ikut mati, no orphan.
// ---------------------------------------------------------------------------

type child struct {
	name string
	cmd  *exec.Cmd
	pid  int
	done chan error // buffer 1, dikirim waiter setelah Wait() kembali

	mu      sync.Mutex
	exited  bool
	exitErr error
}

func (ch *child) state() (bool, error) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.exited, ch.exitErr
}

// launch menjalankan cmd sebagai child torctl dan mencatat pid file.
func launch(name, pidPath string, cmd *exec.Cmd) (*child, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
	ch := &child{name: name, cmd: cmd, done: make(chan error, 1)}
	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		ch.pid = cmd.Process.Pid
		writePID(pidPath, ch.pid)
		started <- nil
		err := cmd.Wait() // mereap; sekalian menahan thread selama anak hidup
		ch.mu.Lock()
		ch.exited, ch.exitErr = true, err
		ch.mu.Unlock()
		if readPID(pidPath) == ch.pid {
			removePID(pidPath)
		}
		ch.done <- err
	}()
	if err := <-started; err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return ch, nil
}

// stopChild menghentikan anak dengan urutan SIGTERM (grace) → SIGKILL.
func stopChild(ch *child, grace time.Duration) {
	if ch == nil || ch.pid <= 0 {
		return
	}
	if exited, _ := ch.state(); exited {
		return
	}
	killPID(ch.pid, syscall.SIGTERM, true)
	select {
	case <-ch.done:
	case <-time.After(grace):
		killPID(ch.pid, syscall.SIGKILL, true)
		<-ch.done
	}
}

// ---------------------------------------------------------------------------
// Snowflake (dijalankan torctl sendiri, mode PT client eksternal)
// ---------------------------------------------------------------------------

// startSnowflake menjalankan snowflake-client dengan environment protokol PT
// (TOR_PT_MANAGED_TRANSPORT_VER / TOR_PT_CLIENT_TRANSPORTS), lalu membaca
// baris "CMETHOD snowflake SOCKS5 ip:port" dari stdout untuk mengetahui
// alamat SOCKS lokalnya. Alamat itulah yang ditulis ke torrc tor.
func startSnowflake(c *Config, abort <-chan os.Signal) (*child, string, error) {
	args := []string{}
	if c.SnowflakeURL != "" {
		args = append(args, "-url", c.SnowflakeURL)
	}
	if c.SnowflakeList != "" {
		args = append(args, "-fronts", c.SnowflakeList)
	}
	if c.SnowflakeICE != "" {
		args = append(args, "-ice", c.SnowflakeICE)
	}
	args = append(args, "-log", c.sfLogPath())

	cmd := exec.Command(c.SnowflakeBin, args...)
	cmd.Env = ptEnv()

	logFile, err := os.OpenFile(c.sfLogPath(),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, "", fmt.Errorf("snowflake log: %w", err)
	}
	cmd.Stderr = logFile

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logFile.Close()
		return nil, "", err
	}

	log.Printf("starting snowflake (%s)...", c.SnowflakeBin)
	ch, err := launch("snowflake", c.sfPIDPath(), cmd)
	if err != nil {
		logFile.Close()
		return nil, "", fmt.Errorf(
			"%w\npasang snowflake-client dulu, atau set TORCTL_SNOWFLAKE=0", err)
	}

	// Parser CMETHOD + lanjutkan sisa stdout ke file log (agar pipe tidak penuh).
	type cmResult struct {
		addr string
		err  error
	}
	resCh := make(chan cmResult, 2)
	go func() {
		sc := bufio.NewScanner(stdout)
		var envErr string
		for sc.Scan() {
			line := sc.Text()
			logFile.WriteString(line + "\n")
			if strings.HasPrefix(line, "CMETHOD snowflake ") {
				f := strings.Fields(line)
				if len(f) >= 4 {
					resCh <- cmResult{addr: f[3]}
					continue // tetap salin baris berikutnya ke log
				}
			}
			if strings.HasPrefix(line, "ENV-ERROR") {
				envErr = line
			}
		}
		if envErr != "" {
			resCh <- cmResult{err: fmt.Errorf("snowflake: %s", envErr)}
		} else {
			resCh <- cmResult{err: fmt.Errorf("snowflake keluar sebelum CMETHOD")}
		}
	}()

	select {
	case sig := <-abort:
		stopChild(ch, 3*time.Second)
		logFile.Close()
		return nil, "", &sigError{sig: sig}
	case res := <-resCh:
		if res.err != nil {
			stopChild(ch, 3*time.Second)
			logFile.Close()
			return nil, "", res.err
		}
		log.Printf("snowflake socks listener: %s", res.addr)
		return ch, res.addr, nil
	case <-time.After(15 * time.Second):
		stopChild(ch, 3*time.Second)
		logFile.Close()
		return nil, "", fmt.Errorf(
			"snowflake tidak mengeluarkan CMETHOD dalam 15s (cek -log %s)", c.sfLogPath())
	}
}

// ptEnv membuang variabel TOR_PT_* yang mewarisi sesi lama, lalu menyetel
// environment protokol Managed Transport untuk mode client.
func ptEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "TOR_PT_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"TOR_PT_MANAGED_TRANSPORT_VER=1",
		"TOR_PT_CLIENT_TRANSPORTS=snowflake")
}

// ---------------------------------------------------------------------------
// Tor
// ---------------------------------------------------------------------------

// writeTorrc menggenerate torrc untuk sesi ini. Mode snowflake memakai
// ClientTransportPlugin ... socks5 (mode eksternal): tor tidak spawn
// snowflake sendiri — snowflake adalah child torctl.
func writeTorrc(c *Config, sfAddr string) error {
	bridges := defaultBridges(c)
	pluginNeeded := false
	for _, b := range bridges {
		if strings.HasPrefix(b, "snowflake ") {
			pluginNeeded = true
		}
	}
	transport := func(b string) string {
		f := strings.Fields(b)
		if len(f) == 0 {
			return ""
		}
		t := f[0]
		// bridge polos tanpa transport: "1.2.3.4:9001 FPRINT" atau "ip:port"
		if _, _, err := netSplitHostPortOK(t); err == nil {
			return ""
		}
		return t
	}
	for _, b := range bridges {
		t := transport(b)
		if t == "" || t == "snowflake" {
			continue
		}
		return fmt.Errorf(
			"transport %q tidak didukung torctl (hanya snowflake): %s", t, b)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "# generated by torctl — jangan diedit, dibuat ulang tiap start\n")
	fmt.Fprintf(&sb, "DataDirectory %s\n", c.dataDir())
	fmt.Fprintf(&sb, "SocksPort %s\n", c.SocksAddr)

	if pluginNeeded {
		if !c.Snowflake {
			return fmt.Errorf("bridge snowflake dipakai tapi TORCTL_SNOWFLAKE=0")
		}
		if sfAddr == "" {
			return fmt.Errorf("snowflake belum siap (alamat SOCKS kosong)")
		}
		fmt.Fprintf(&sb, "UseBridges 1\n")
		fmt.Fprintf(&sb, "ClientTransportPlugin snowflake socks5 %s\n", sfAddr)
	} else if len(bridges) > 0 {
		fmt.Fprintf(&sb, "UseBridges 1\n")
	}
	for _, b := range bridges {
		fmt.Fprintf(&sb, "Bridge %s\n", b)
	}
	if extra := os.Getenv("TORCTL_EXTRA_TORRC"); extra != "" {
		fmt.Fprintf(&sb, "Include %s\n", extra)
	}
	return os.WriteFile(c.torrcPath(), []byte(sb.String()), 0644)
}

// defaultBridges: line snowflake bawaan (dipakai bila snowflake aktif dan
// TORCTL_BRIDGES tidak diisi).
func defaultBridges(c *Config) []string {
	if len(c.Bridges) > 0 {
		return c.Bridges
	}
	if c.Snowflake {
		return []string{
			"snowflake 192.0.2.3:1 2 B8DA951F1B4652FB6945D394F2D6487AB75B2668",
		}
	}
	return nil
}

// startTor menjalankan tor (child), memotong log ke tor.log, lalu menunggu
// "Bootstrapped 100%" (maksimum bootstrapTimeout).
func startTor(c *Config, abort <-chan os.Signal) (*child, error) {
	// bersihkan sisa state pluggable transport lama
	os.RemoveAll(filepath.Join(c.dataDir(), "pt_state"))

	f, err := os.OpenFile(c.torLogPath(),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return nil, fmt.Errorf("buka tor.log: %w", err)
	}
	defer f.Close()

	cmd := exec.Command(c.TorBin, "-f", c.torrcPath())
	cmd.Stdout = f
	cmd.Stderr = f

	ch, err := launch("tor", c.torPIDPath(), cmd)
	if err != nil {
		return nil, err
	}
	log.Printf("tor started (pid %d), waiting bootstrap...", ch.pid)

	deadline := time.After(c.BootstrapTimeout)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case sig := <-abort:
			return ch, &sigError{sig: sig}
		case <-deadline:
			return ch, fmt.Errorf("tor bootstrap timeout after %s", c.BootstrapTimeout)
		case <-tick.C:
			if exited, err := ch.state(); exited {
				return ch, fmt.Errorf("tor exited prematurely (%v), cek %s",
					err, c.torLogPath())
			}
			if bootstrapDone(c) {
				log.Print("tor bootstrapped 100%")
				return ch, nil
			}
		}
	}
}

func bootstrapDone(c *Config) bool {
	b, err := os.ReadFile(c.torLogPath())
	if err != nil {
		return false
	}
	return strings.Contains(string(b), "Bootstrapped 100%")
}

// ---------------------------------------------------------------------------
// util kecil
// ---------------------------------------------------------------------------

// netSplitHostPortOK memisahkan host:port dan memvalidasi portnya —
// dipakai untuk mengenali apakah token pertama bridge adalah addr:port
// (tanpa transport) atau nama transport.
func netSplitHostPortOK(s string) (string, string, error) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", "", fmt.Errorf("no port")
	}
	host, port := s[:i], s[i+1:]
	if host == "" || port == "" {
		return "", "", fmt.Errorf("bad addr")
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", "", fmt.Errorf("bad port")
	}
	return host, port, nil
}
