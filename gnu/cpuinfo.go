package gnu

import (
	"fmt"
	"log"
	"runtime"
	"strconv"
	"strings"

	"github.com/rosenlo/toolkits/file"
)

type CPU struct {
	Num     int
	MHz     int
	Module  string
	Flags   string
	Virtual bool
}

func (c *CPU) String() {
	fmt.Sprintf("<Module: %s, Num: %d, MHz: %d, Virtual: %t, Flags: %s>", c.Module, c.Num, c.MHz, c.Virtual, c.Flags)
}

func NewCPU() *CPU {
	return &CPU{Num: runtime.NumCPU()}
}

func CpuInfo() (*CPU, error) {
	lines, err := file.ReadLines("/proc/cpuinfo")
	if err != nil {
		return nil, err
	}
	cpu := NewCPU()
	for _, line := range lines {
		fields := strings.Split(line, ":")
		if len(fields) < 2 {
			continue
		}
		switch strings.TrimSpace(fields[0]) {
		case "flags":
			cpu.Virtual = strings.Contains(fields[1], "hypervisor")
		case "model name":
			cpu.Module = strings.TrimSpace(fields[1])
		case "cpu MHz":
			_fields := strings.Split(fields[1], ".")
			if len(_fields) > 0 {
				cpu.MHz, err = strconv.Atoi(strings.TrimSpace(_fields[0]))
				if err != nil {
					log.Println(err)
					cpu.MHz = 1
				}
			}
		}
	}
	return cpu, nil
}
