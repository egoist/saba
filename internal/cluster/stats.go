package cluster

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/egoist/saba/internal/api"
)

// HostStats reads the machine's load, memory and the disk holding dir (or /
// when dir does not exist).
func HostStats(dir string) api.HostStats {
	s := api.HostStats{CPUs: runtime.NumCPU()}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			s.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		var total, avail uint64
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 2 {
				continue
			}
			kb, _ := strconv.ParseUint(fields[1], 10, 64)
			switch fields[0] {
			case "MemTotal:":
				total = kb * 1024
			case "MemAvailable:":
				avail = kb * 1024
			}
		}
		f.Close()
		if total > 0 && avail <= total {
			s.MemTotal, s.MemUsed = total, total-avail
		}
	}
	if _, err := os.Stat(dir); err != nil {
		dir = "/"
	}
	var fs syscall.Statfs_t
	if syscall.Statfs(dir, &fs) == nil && fs.Blocks > 0 {
		s.DiskTotal = fs.Blocks * uint64(fs.Bsize)
		s.DiskUsed = (fs.Blocks - fs.Bavail) * uint64(fs.Bsize)
	}
	return s
}
