package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

type procEntry struct {
	PID      int
	PPID     int
	Name     string
	RSSBytes uint64
}

type processRSSSample struct {
	PID      int    `json:"pid"`
	PPID     int    `json:"ppid"`
	Name     string `json:"name"`
	RSSBytes uint64 `json:"rss_bytes"`
}

type processTreeRSS struct {
	TotalRSSBytes     uint64             `json:"total_rss_bytes"`
	RootRSSBytes      uint64             `json:"root_rss_bytes"`
	ComponentRSSBytes map[string]uint64  `json:"component_rss_bytes"`
	Processes         []processRSSSample `json:"processes"`
}

func sampleProcessTreeRSS(rootPID int) (processTreeRSS, error) {
	if rootPID <= 0 {
		return processTreeRSS{}, errors.New("root pid must be positive")
	}
	var entries []procEntry
	var err error
	if runtime.GOOS == "linux" {
		entries, err = readLinuxProcTable("/proc")
	} else {
		entries, err = readPSProcTable()
	}
	if err != nil {
		return processTreeRSS{}, err
	}
	return aggregateProcessTreeRSS(rootPID, entries)
}

func readLinuxProcTable(procRoot string) ([]procEntry, error) {
	dirEntries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", procRoot, err)
	}
	entries := make([]procEntry, 0, len(dirEntries))
	for _, entry := range dirEntries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		statusData, err := os.ReadFile(filepath.Join(procRoot, entry.Name(), "status"))
		if err != nil {
			continue
		}
		name, ppid, rssBytes, ok := parseProcStatus(statusData)
		if !ok {
			continue
		}
		if cmdlineData, cmdErr := os.ReadFile(filepath.Join(procRoot, entry.Name(), "cmdline")); cmdErr == nil {
			if argv0 := parseProcCmdlineName(cmdlineData); argv0 != "" {
				name = argv0
			}
		}
		entries = append(entries, procEntry{
			PID:      pid,
			PPID:     ppid,
			Name:     normalizeProcessName(name),
			RSSBytes: rssBytes,
		})
	}
	return entries, nil
}

func parseProcCmdlineName(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	if idx := bytes.IndexByte(data, 0); idx >= 0 {
		data = data[:idx]
	}
	return normalizeProcessName(string(data))
}

func parseProcStatus(data []byte) (string, int, uint64, bool) {
	var name string
	var ppid int
	var hasPPID bool
	var rssBytes uint64
	var hasRSS bool
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "Name:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
		case strings.HasPrefix(line, "PPid:"):
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if parsed, err := strconv.Atoi(fields[1]); err == nil && parsed >= 0 {
					ppid = parsed
					hasPPID = true
				}
			}
		case strings.HasPrefix(line, "VmRSS:"):
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
					rssBytes = kb * 1024
					hasRSS = true
				}
			}
		}
	}
	if !hasPPID || !hasRSS {
		return "", 0, 0, false
	}
	return normalizeProcessName(name), ppid, rssBytes, true
}

func readPSProcTable() ([]procEntry, error) {
	out, err := exec.Command("ps", "-eo", "pid=,ppid=,rss=,comm=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps process table: %w", err)
	}
	return parsePSProcessTable(out), nil
}

func parsePSProcessTable(data []byte) []procEntry {
	lines := strings.Split(string(data), "\n")
	entries := make([]procEntry, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		rssKB, err3 := strconv.ParseUint(fields[2], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || pid <= 0 || ppid < 0 {
			continue
		}
		name := normalizeProcessName(strings.Join(fields[3:], " "))
		entries = append(entries, procEntry{
			PID:      pid,
			PPID:     ppid,
			Name:     name,
			RSSBytes: rssKB * 1024,
		})
	}
	return entries
}

func aggregateProcessTreeRSS(rootPID int, entries []procEntry) (processTreeRSS, error) {
	byPID := make(map[int]procEntry, len(entries))
	children := make(map[int][]int, len(entries))
	for _, e := range entries {
		byPID[e.PID] = e
		if e.PPID > 0 && e.PPID != e.PID {
			children[e.PPID] = append(children[e.PPID], e.PID)
		}
	}
	root, ok := byPID[rootPID]
	if !ok {
		return processTreeRSS{}, fmt.Errorf("root process %d not found in process table", rootPID)
	}
	visited := make(map[int]struct{})
	queue := []int{rootPID}
	visited[rootPID] = struct{}{}

	result := processTreeRSS{
		RootRSSBytes:      root.RSSBytes,
		ComponentRSSBytes: make(map[string]uint64),
	}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		proc, exists := byPID[pid]
		if !exists {
			continue
		}
		name := normalizeProcessName(proc.Name)
		if name == "" {
			name = fmt.Sprintf("pid-%d", proc.PID)
		}
		result.TotalRSSBytes += proc.RSSBytes
		result.ComponentRSSBytes[name] += proc.RSSBytes
		result.Processes = append(result.Processes, processRSSSample{
			PID:      proc.PID,
			PPID:     proc.PPID,
			Name:     name,
			RSSBytes: proc.RSSBytes,
		})
		for _, childPID := range children[pid] {
			if _, seen := visited[childPID]; !seen {
				visited[childPID] = struct{}{}
				queue = append(queue, childPID)
			}
		}
	}
	sort.Slice(result.Processes, func(i, j int) bool {
		if result.Processes[i].PID == rootPID {
			return true
		}
		if result.Processes[j].PID == rootPID {
			return false
		}
		if result.Processes[i].Name != result.Processes[j].Name {
			return result.Processes[i].Name < result.Processes[j].Name
		}
		return result.Processes[i].PID < result.Processes[j].PID
	})
	return result, nil
}

func normalizeProcessName(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	return filepath.Base(trimmed)
}
