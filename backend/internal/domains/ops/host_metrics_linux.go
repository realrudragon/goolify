//go:build linux

package ops

import (
	"context"
	"errors"
	"os"
	"sync"
	"syscall"
	"time"
)

var (
	cpuSampleMu   sync.Mutex
	prevCPUIdle   uint64
	prevCPUTotal  uint64
	hasPrevSample bool
)

// CollectLocalHostMetrics reads real CPU, memory, and root disk utilization from Linux /proc and statfs.
func CollectLocalHostMetrics(ctx context.Context) (HostMetricsSample, error) {
	cpuPct, err := sampleLinuxCPU(ctx)
	if err != nil {
		return HostMetricsSample{}, err
	}
	memRaw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return HostMetricsSample{}, err
	}
	memPct, ok := parseProcMeminfo(string(memRaw))
	if !ok {
		return HostMetricsSample{}, errors.New("could not parse /proc/meminfo")
	}

	var diskPct float64
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err == nil && st.Blocks > 0 {
		used := float64(st.Blocks - st.Bfree)
		diskPct = clampPercent((used / float64(st.Blocks)) * 100.0)
	}

	return HostMetricsSample{
		CPUPercent:    cpuPct,
		MemoryPercent: memPct,
		DiskPercent:   diskPct,
		RecordedAt:    time.Now().UTC(),
	}, nil
}

func sampleLinuxCPU(ctx context.Context) (float64, error) {
	raw1, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, err
	}
	idle1, total1, ok := parseProcStatCPU(string(raw1))
	if !ok {
		return 0, errors.New("could not parse /proc/stat")
	}

	cpuSampleMu.Lock()
	if hasPrevSample && total1 > prevCPUTotal {
		dt := total1 - prevCPUTotal
		di := uint64(0)
		if idle1 >= prevCPUIdle {
			di = idle1 - prevCPUIdle
		}
		prevCPUIdle = idle1
		prevCPUTotal = total1
		cpuSampleMu.Unlock()
		return clampPercent((float64(dt-di) / float64(dt)) * 100.0), nil
	}
	cpuSampleMu.Unlock()

	// First call: take a short 150ms delta sample so we immediately return a real CPU reading.
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(150 * time.Millisecond):
	}
	raw2, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, err
	}
	idle2, total2, ok := parseProcStatCPU(string(raw2))
	if !ok {
		return 0, errors.New("could not parse /proc/stat")
	}

	cpuSampleMu.Lock()
	prevCPUIdle = idle2
	prevCPUTotal = total2
	hasPrevSample = true
	cpuSampleMu.Unlock()

	if total2 <= total1 {
		return 0, nil
	}
	dt := total2 - total1
	di := uint64(0)
	if idle2 >= idle1 {
		di = idle2 - idle1
	}
	return clampPercent((float64(dt-di) / float64(dt)) * 100.0), nil
}
