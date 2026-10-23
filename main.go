package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const usageText = `usage: torctl {start|stop|restart|status|logs}

perintah:
  start     nyalakan snowflake + Tor (tunggu bootstrap 100%) + HTTP bridge
            (foreground — biarkan sesi ini terbuka)
  stop      matikan bridge -> tor -> snowflake, bersihkan state (dari sesi lain)
  restart   stop lalu start
  status    PID, port, dan IP keluar Tor
  logs      tail log tor + bridge + snowflake
            (-f follow, -n jumlah baris)

konfigurasi (environment):
  TORCTL_SNOWFLAKE=0         nonaktifkan snowflake (plain Tor)
  TORCTL_BRIDGES=...         bridge custom, dipisah ";" / newline
  TORCTL_SOCKS_ADDR          default 127.0.0.1:9050
  TORCTL_BRIDGE_ADDR         default 127.0.0.1:8118
  TORCTL_TOR_BIN             default: tor
  TORCTL_SNOWFLAKE_BIN       default: snowflake-client
  TORCTL_SNOWFLAKE_URL       broker snowflake
  TORCTL_SNOWFLAKE_FRONTS    front domain (dipisah koma), opsional
  TORCTL_SNOWFLAKE_ICE       daftar STUN
  TORCTL_PREFIX / PREFIX     lokasi prefix ($PREFIX/var/{run,log,lib}/torctl)
`

// sigError ditulis kembali dari fase startup bila sinyal tiba di tengah jalan.
type sigError struct{ sig os.Signal }

func (e *sigError) Error() string { return "sinyal " + e.sig.String() }

func exeName() string {
	p, err := os.Executable()
	if err != nil {
		return "torctl"
	}
	return filepath.Base(p)
}

// ---------------------------------------------------------------------------
// torctl start — supervisor foreground
// ---------------------------------------------------------------------------

func cmdStart(c *Config) {
	if err := c.ensureDirs(); err != nil {
		log.Fatalf("buat direktori: %v", err)
	}

	// preflight: tolak start ganda
	if sup := readPID(c.torctlPIDPath()); sup > 0 && pidAlive(sup) {
		if pidMatches(sup, exeName()) {
			log.Fatalf("torctl sudah berjalan (pid %d) — pakai: torctl stop | torctl restart", sup)
		}
		fmt.Fprintf(os.Stderr,
			"torctl: torctl.pid basi (%d kini milik %s), dibuang\n",
			sup, procCmdline(sup))
		removePID(c.torctlPIDPath())
	}
	// anak yatim dari sesi lama yang crash → bersihkan dulu
	stopPIDFile(c.sfPIDPath(), c.SnowflakeBin, true, 3*time.Second)
	stopPIDFile(c.torPIDPath(), c.TorBin, true, 5*time.Second)

	abort := make(chan os.Signal, 2)
	signal.Notify(abort, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(abort)

	writePID(c.torctlPIDPath(), os.Getpid())

	var torCh, sfCh *child
	// urutan stop sesuai desain: bridge -> tor -> snowflake -> state
	shutdown := func() {
		stopBridge()
		stopChild(torCh, 8*time.Second)
		stopChild(sfCh, 5*time.Second)
		os.RemoveAll(c.RunDir)
	}
	stoppedCleanly := func(err error) bool {
		if _, ok := err.(*sigError); ok {
			shutdown()
			log.Print("stopped")
			return true
		}
		return false
	}

	var sfAddr string
	if c.Snowflake {
		ch, addr, err := startSnowflake(c, abort)
		if err != nil {
			if stoppedCleanly(err) {
				return
			}
			shutdown()
			log.Fatalf("start snowflake: %v", err)
		}
		sfCh, sfAddr = ch, addr
	}

	if err := writeTorrc(c, sfAddr); err != nil {
		shutdown()
		log.Fatalf("torrc: %v", err)
	}

	var err error
	torCh, err = startTor(c, abort)
	if err != nil {
		if stoppedCleanly(err) {
			return
		}
		shutdown()
		log.Fatalf("start tor: %v", err)
	}

	if err := startBridge(c); err != nil {
		shutdown()
		log.Fatalf("start bridge: %v", err)
	}
	log.Printf("bridge listening on %s -> %s", c.BridgeAddr, c.SocksAddr)

	// event loop: sinyal shutdown / anak mati mendadak
	var sfDone chan error
	if sfCh != nil {
		sfDone = sfCh.done
	}
	select {
	case <-abort:
		shutdown()
		log.Print("stopped")
	case err := <-torCh.done:
		log.Printf("tor keluar tak terduga: %v", err)
		shutdown()
		os.Exit(1)
	case err := <-sfDone:
		log.Printf("snowflake keluar tak terduga: %v", err)
		shutdown()
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// torctl stop
// ---------------------------------------------------------------------------

func cmdStop(c *Config) {
	if sup := readPID(c.torctlPIDPath()); sup > 0 && pidAlive(sup) {
		if sup != os.Getpid() && pidMatches(sup, exeName()) {
			log.Printf("mengirim SIGTERM ke torctl (pid %d)...", sup)
			syscall.Kill(sup, syscall.SIGTERM)
			for i := 0; i < 100 && pidAlive(sup); i++ {
				time.Sleep(100 * time.Millisecond)
			}
			if pidAlive(sup) {
				log.Printf("torctl tidak merespons, SIGKILL...")
				syscall.Kill(sup, syscall.SIGKILL)
				time.Sleep(300 * time.Millisecond)
			}
		} else {
			fmt.Fprintf(os.Stderr,
				"torctl: torctl.pid %d kini milik proses lain (%s), tidak disentuh\n",
				sup, procCmdline(sup))
			removePID(c.torctlPIDPath())
		}
	}
	// sapu bersih anak yang tersisa (mis. supervisor crash tanpa pdeathsig)
	stopPIDFile(c.torPIDPath(), c.TorBin, true, 5*time.Second)
	stopPIDFile(c.sfPIDPath(), c.SnowflakeBin, true, 3*time.Second)
	os.RemoveAll(c.RunDir)
	log.Println("stopped")
}

// ---------------------------------------------------------------------------
// torctl status
// ---------------------------------------------------------------------------

func cmdStatus(c *Config) {
	fmt.Printf("torctl pid  : %s\n", pidLine(readPID(c.torctlPIDPath())))
	fmt.Printf("tor pid     : %s\n", pidLine(readPID(c.torPIDPath())))
	if c.Snowflake {
		fmt.Printf("snowflake   : %s\n", pidLine(readPID(c.sfPIDPath())))
	} else {
		fmt.Printf("snowflake   : off\n")
	}
	socksOK := portListening(c.SocksAddr)
	bridgeOK := portListening(c.BridgeAddr)
	fmt.Printf("socks       : %s listening=%v\n", c.SocksAddr, socksOK)
	fmt.Printf("bridge      : %s listening=%v (in-process torctl)\n", c.BridgeAddr, bridgeOK)
	if !socksOK {
		fmt.Printf("tor check   : (lewati — SOCKS %s tidak aktif)\n", c.SocksAddr)
		return
	}
	body, err := checkTorViaSOCKS(c.SocksAddr, c.CheckURL)
	if err != nil {
		fmt.Printf("tor check   : ERROR %v\n", err)
		return
	}
	fmt.Printf("tor check   : %s\n", body)
}

// ---------------------------------------------------------------------------
// torctl logs
// ---------------------------------------------------------------------------

func cmdLogs(c *Config, args []string) {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	follow := fs.Bool("f", false, "ikut log terus (follow)")
	n := fs.Int("n", 30, "jumlah baris tail")
	fs.Parse(args)

	targets := []struct{ name, path string }{
		{"tor", c.torLogPath()},
		{"bridge", c.bridgeLogPath()},
		{"snowflake", c.sfLogPath()},
	}

	if !*follow {
		for _, t := range targets {
			fmt.Printf("=== %s.log (tail) ===\n", t.name)
			tailFile(t.path, *n)
		}
		return
	}

	// follow: mulai dari ujung file, tampilkan baris baru dengan prefix nama
	tails := make([]*fileTail, 0, len(targets))
	for _, t := range targets {
		ft := &fileTail{name: t.name, path: t.path}
		if st, err := os.Stat(t.path); err == nil {
			ft.off = st.Size()
		}
		tails = append(tails, ft)
	}
	abort := make(chan os.Signal, 1)
	signal.Notify(abort, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(abort)
	fmt.Println("mengikuti log (Ctrl+C untuk berhenti)...")

	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-abort:
			fmt.Println("(berhenti)")
			return
		case <-tick.C:
			for _, ft := range tails {
				lines, err := ft.readTailLines()
				if err != nil {
					continue // file belum ada / sementara hilang
				}
				for _, l := range lines {
					fmt.Printf("[%s] %s\n", ft.name, l)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usageText)
		os.Exit(1)
	}
	c := loadConfig()
	switch os.Args[1] {
	case "start":
		cmdStart(c)
	case "stop":
		cmdStop(c)
	case "restart":
		cmdStop(c)
		time.Sleep(700 * time.Millisecond)
		cmdStart(c)
	case "status":
		cmdStatus(c)
	case "logs":
		cmdLogs(c, os.Args[2:])
	default:
		fmt.Println("unknown command:", os.Args[1])
		fmt.Print(usageText)
		os.Exit(1)
	}
}
