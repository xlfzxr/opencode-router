// Package service controls a single Linux/Termux process without Node or systemd.
package service

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type state struct {
	PID   int    `json:"pid"`
	Start string `json:"started"`
}

func identity(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(b)[strings.LastIndex(string(b), ")")+1:])
	if len(fields) < 20 || fields[0] == "Z" {
		return ""
	}
	return fields[19]
}
func read(file string) state {
	var s state
	b, _ := os.ReadFile(file)
	_ = json.Unmarshal(b, &s)
	return s
}
func alive(s state) bool { return s.Start != "" && identity(s.PID) == s.Start }
func directory() string {
	if p := os.Getenv("OCR_SERVICE_DIR"); p != "" {
		return p
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".local/state/opencode-router/service")
}

// Run returns false when args are ordinary server flags. ready is used only by
// the detached child: the parent receives success after the HTTP socket binds.
func Run(args []string) (handled bool, err error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return false, nil
	}
	cmd := args[0]
	if cmd == "foreground" {
		return false, nil
	}
	if cmd != "start" && cmd != "stop" && cmd != "restart" && cmd != "status" && cmd != "logs" {
		return true, fmt.Errorf("usage: service {start|stop|restart|status|logs|foreground}")
	}
	dir := directory()
	if err = os.MkdirAll(dir, 0700); err != nil {
		return true, err
	}
	if cmd == "logs" {
		c := exec.Command("tail", "-n", "100", "-f", filepath.Join(dir, "service.log"))
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		return true, c.Run()
	}
	lock, err := os.OpenFile(filepath.Join(dir, "control.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return true, err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return true, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	file := filepath.Join(dir, "service.json")
	stop := func() error {
		s := read(file)
		if !alive(s) {
			fmt.Println("Service tidak berjalan.")
			return nil
		}
		if err := syscall.Kill(s.PID, syscall.SIGTERM); err != nil {
			return err
		}
		for i := 0; i < 350; i++ {
			if !alive(s) {
				os.Remove(file)
				fmt.Println("Service berhenti.")
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		// Never signal a reused PID.
		if alive(s) {
			return fmt.Errorf("stop timeout; periksa service logs")
		}
		return nil
	}
	switch cmd {
	case "status":
		s := read(file)
		if !alive(s) {
			return true, fmt.Errorf("service tidak berjalan")
		}
		fmt.Printf("Service berjalan (PID %d).\n", s.PID)
		return true, nil
	case "stop":
		return true, stop()
	case "restart":
		if err := stop(); err != nil {
			return true, err
		}
	}
	if s := read(file); alive(s) {
		fmt.Printf("Service sudah berjalan (PID %d).\n", s.PID)
		return true, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return true, err
	}
	log, err := os.OpenFile(filepath.Join(dir, "service.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return true, err
	}
	defer log.Close()
	reader, writer, err := os.Pipe()
	if err != nil {
		return true, err
	}
	defer reader.Close()
	child := exec.Command(executable, append([]string{"foreground"}, args[1:]...)...)
	child.Env = append(os.Environ(), "OCR_READY_FD=3")
	child.ExtraFiles = []*os.File{writer}
	child.Stdout = log
	child.Stderr = log
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = child.Start(); err != nil {
		writer.Close()
		return true, err
	}
	writer.Close()
	s := state{PID: child.Process.Pid, Start: identity(child.Process.Pid)}
	b, _ := json.Marshal(s)
	if err = os.WriteFile(file, b, 0600); err != nil {
		child.Process.Kill()
		child.Wait()
		return true, err
	}
	_ = reader.SetReadDeadline(time.Now().Add(15 * time.Second))
	ready, err := io.ReadAll(reader)
	if err != nil || string(ready) != "ready" {
		if alive(s) {
			child.Process.Kill()
		}
		child.Wait()
		os.Remove(file)
		return true, fmt.Errorf("startup gagal; lihat %s", filepath.Join(dir, "service.log"))
	}
	child.Process.Release()
	fmt.Printf("Service berjalan (PID %d). Log: %s\n", s.PID, filepath.Join(dir, "service.log"))
	return true, nil
}

// Ready is called after net.Listen succeeds, not merely after spawning.
func Ready() {
	if value := os.Getenv("OCR_READY_FD"); value != "" {
		fd, err := strconv.Atoi(value)
		if err == nil {
			f := os.NewFile(uintptr(fd), "ready")
			if f != nil {
				_, _ = f.WriteString("ready")
				_ = f.Close()
			}
		}
		os.Unsetenv("OCR_READY_FD")
	}
}
