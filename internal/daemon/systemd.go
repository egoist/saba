package daemon

import (
	"net"
	"os"
	"strconv"
	"syscall"
)

// inheritedListeners returns the listening sockets handed over by systemd
// socket activation (LISTEN_FDS protocol), if any.
//
// With socket activation the listening sockets belong to systemd and outlive
// the daemon. While the daemon restarts, the kernel keeps accepting
// connections into the sockets' backlog, so clients wait for a moment instead
// of getting "connection refused".
func inheritedListeners() []net.Listener {
	defer func() {
		// Never leak the protocol variables into child processes (docker, git).
		os.Unsetenv("LISTEN_PID")
		os.Unsetenv("LISTEN_FDS")
		os.Unsetenv("LISTEN_FDNAMES")
	}()
	if pid, err := strconv.Atoi(os.Getenv("LISTEN_PID")); err != nil || pid != os.Getpid() {
		return nil
	}
	n, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if err != nil || n <= 0 {
		return nil
	}
	const firstFD = 3
	var out []net.Listener
	for fd := firstFD; fd < firstFD+n; fd++ {
		syscall.CloseOnExec(fd)
		f := os.NewFile(uintptr(fd), "systemd-socket-"+strconv.Itoa(fd))
		l, err := net.FileListener(f) // duplicates the descriptor
		f.Close()
		if err == nil {
			out = append(out, l)
		}
	}
	return out
}

// takeListener removes and returns the inherited listener bound to addr's
// port, or nil if there is none.
func takeListener(listeners *[]net.Listener, addr string) net.Listener {
	_, want, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	for i, l := range *listeners {
		if _, port, err := net.SplitHostPort(l.Addr().String()); err == nil && port == want {
			*listeners = append((*listeners)[:i], (*listeners)[i+1:]...)
			return l
		}
	}
	return nil
}
