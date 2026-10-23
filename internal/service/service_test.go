package service

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("OCR_SERVICE_HELPER") == "1" {
		handled, err := Run(os.Args[1:])
		if handled {
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
		if os.Getenv("OCR_FAIL_READY") == "1" {
			os.Exit(2)
		}
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
		Ready()
		fmt.Println("helper ready")
		<-ch
		fmt.Println("graceful shutdown")
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func TestLifecycle(t *testing.T) {
	dir := t.TempDir()
	exe, _ := os.Executable()
	run := func(command string, extra ...string) (string, error) {
		c := exec.Command(exe, command)
		c.Env = append(os.Environ(), "OCR_SERVICE_HELPER=1", "OCR_SERVICE_DIR="+dir, "GORACE=atexit_sleep_ms=0")
		c.Env = append(c.Env, extra...)
		b, err := c.CombinedOutput()
		return string(b), err
	}
	defer run("stop")
	if _, err := run("status"); err == nil {
		t.Fatal("stopped status succeeded")
	}
	if out, err := run("start"); err != nil {
		t.Fatalf("start: %s %v", out, err)
	}
	first := read(filepath.Join(dir, "service.json"))
	if !alive(first) {
		t.Fatal("no child")
	}
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", first.PID))
	fields := strings.Fields(string(b)[strings.LastIndex(string(b), ")")+1:])
	if fields[3] != fmt.Sprint(first.PID) {
		t.Fatalf("child not session leader: %s", b)
	}
	if _, err := run("start"); err != nil {
		t.Fatal(err)
	}
	if next := read(filepath.Join(dir, "service.json")); next != first {
		t.Fatal("duplicate child")
	}
	if _, err := run("status"); err != nil {
		t.Fatal(err)
	}
	if out, err := run("restart"); err != nil {
		t.Fatalf("restart: %s %v", out, err)
	}
	if next := read(filepath.Join(dir, "service.json")); next == first {
		t.Fatal("restart kept PID")
	}
	if out, err := run("stop"); err != nil {
		t.Fatalf("stop: %s %v", out, err)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "service.log"))
	if !strings.Contains(string(log), "graceful shutdown") {
		t.Fatalf("not graceful: %s", log)
	}
	if _, err := run("stop"); err != nil {
		t.Fatal(err)
	}
	if _, err := run("start", "OCR_FAIL_READY=1"); err == nil {
		t.Fatal("failed startup reported success")
	}
	if _, err := run("status"); err == nil {
		t.Fatal("failed startup left live state")
	}
}
func TestPIDReuseGuard(t *testing.T) {
	if alive(state{PID: os.Getpid(), Start: "wrong"}) {
		t.Fatal("PID reuse guard failed")
	}
}
