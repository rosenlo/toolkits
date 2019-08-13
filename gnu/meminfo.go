package gnu

import (
	"strconv"
	"strings"

	"github.com/rosenlo/toolkits/file"
)

type Mem struct {
	MemTotal     uint64
	MemFree      uint64
	MemAvailable uint64
	Buffers      uint64
	Cached       uint64
	SwapCached   uint64
	SwapTotal    uint64
	SwapFree     uint64
	SwapUsed     uint64
}

var Bit uint64 = 1000

func MemInfo() (*Mem, error) {
	lines, err := file.ReadLines("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	mem := new(Mem)
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		val, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return nil, err
		}

		switch strings.TrimSpace(fields[0]) {
		case "MemTotal:":
			mem.MemTotal = val * Bit
		case "MemFree:":
			mem.MemFree = val * Bit
		case "MemAvailable:":
			mem.MemAvailable = val * Bit
		case "Buffers:":
			mem.Buffers = val * Bit
		case "Cached:":
			mem.Cached = val * Bit
		case "SwapCached:":
			mem.SwapCached = val * Bit
		case "SwapTotal:":
			mem.SwapTotal = val * Bit
		case "SwapFree:":
			mem.SwapFree = val * Bit
		case "SwapUsed:":
			mem.SwapUsed = val * Bit
		}
	}
	return mem, nil
}
