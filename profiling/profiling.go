package profiling

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"
)

func GetUsedPercentage() float64 {
	limit, err := GetMemoryLimit()
	if err != nil {
		log.Printf("Failed to read cgroup memory limit: %v", err)
		return 0.0
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	used := m.Alloc
	usedPercentage := float64(used) / float64(limit)
	return usedPercentage
}

func HeapDump(dumpFile string) {
	f, err := os.Create(dumpFile)
	if err != nil {
		log.Printf("failed to create heap dump file: %v", err)
		return
	}
	defer f.Close()

	if err := pprof.WriteHeapProfile(f); err != nil {
		log.Printf("failed to write heap profile: %v", err)
		return
	}
	log.Printf("Heap dump written to %s", dumpFile)
}

func HeapDumpAndSendToServer(serverURL string) error {
	log.Printf("start dump heap")
	f, err := os.CreateTemp("/tmp", "heapdump-*.out")
	if err != nil {
		return fmt.Errorf("failed to create heap dump file: %w", err)
	}
	defer f.Close()

	names := strings.Split(f.Name(), "/")
	filename := names[len(names)-1]
	path := fmt.Sprintf("%s/%s", serverURL, filename)

	if err := pprof.WriteHeapProfile(f); err != nil {
		return fmt.Errorf("failed to write heap profile: %w", err)
	}

	_, err = f.Seek(0, io.SeekStart)
	if err != nil {
		return fmt.Errorf("failed to seek to start: %w", err)
	}
	req, err := http.NewRequest(http.MethodPut, path, f)
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send heap dump to server: %w", err)
	}

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	log.Printf("Dump heap successfully sent to server %s; response: %s\n", serverURL, string(body))
	return nil
}

func GetMemoryLimit() (uint64, error) {
	data, err := os.ReadFile("/sys/fs/cgroup/memory/memory.limit_in_bytes")
	if err != nil {
		return 0, err
	}
	limit, err := strconv.ParseUint(string(data[:len(data)-1]), 10, 64)
	if err != nil {
		return 0, err
	}
	return limit, nil
}

func MonitorMemoryUsage(memoryUsageThreshold float64, serverURL string, sleepInterval time.Duration) {
	limit, err := GetMemoryLimit()
	if err != nil {
		log.Printf("Failed to read cgroup memory limit: %v", err)
		return
	}
	log.Printf("CGroup Memory Limit: %d bytes\n", limit)

	for {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		used := m.Alloc
		usedPercentage := float64(used) / float64(limit)

		if usedPercentage > memoryUsageThreshold {
			log.Printf("Memory usage: %.2f%% exceeds the threshold %.0f%%.\n", usedPercentage*100, memoryUsageThreshold*100)
			err := HeapDumpAndSendToServer(serverURL)
			if err != nil {
				log.Printf("Error sending heap dump: %v\n", err)
			}
			log.Printf("dump heap done")
		}
		time.Sleep(sleepInterval)
	}
}
