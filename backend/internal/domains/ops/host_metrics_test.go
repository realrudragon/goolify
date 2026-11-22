package ops

import (
	"context"
	"testing"
)

func TestParseProcStatCPU(t *testing.T) {
	sample := "cpu  1000 50 300 8000 200 10 40 0 0 0\ncpu0 500 25 150 4000 100 5 20 0 0 0\n"
	idle, total, ok := parseProcStatCPU(sample)
	if !ok {
		t.Fatal("expected parseProcStatCPU to succeed")
	}
	if idle != 8200 {
		t.Fatalf("expected idle=8200, got %d", idle)
	}
	if total != 9600 {
		t.Fatalf("expected total=9600, got %d", total)
	}
}

func TestParseProcMeminfo(t *testing.T) {
	sample := "MemTotal:       16000000 kB\nMemFree:         2000000 kB\nMemAvailable:    8000000 kB\nBuffers:          500000 kB\nCached:          3500000 kB\n"
	pct, ok := parseProcMeminfo(sample)
	if !ok {
		t.Fatal("expected parseProcMeminfo to succeed")
	}
	if pct != 50.0 {
		t.Fatalf("expected 50.0%%, got %.1f", pct)
	}

	// Fallback when MemAvailable is absent
	legacy := "MemTotal:       10000000 kB\nMemFree:         2000000 kB\nBuffers:         1000000 kB\nCached:          1000000 kB\n"
	pct, ok = parseProcMeminfo(legacy)
	if !ok || pct != 60.0 {
		t.Fatalf("expected 60.0%% fallback, got ok=%v pct=%.1f", ok, pct)
	}
}

func TestParseMetricsTriplet(t *testing.T) {
	s, err := ParseMetricsTriplet("12.34 55.67 80.12\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.CPUPercent != 12.3 || s.MemoryPercent != 55.7 || s.DiskPercent != 80.1 {
		t.Fatalf("unexpected sample: %+v", s)
	}

	s2, err := ParseMetricsTriplet("150 -5 101")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s2.CPUPercent != 100 || s2.MemoryPercent != 0 || s2.DiskPercent != 100 {
		t.Fatalf("expected clamped values, got %+v", s2)
	}
}

func TestCollectLocalHostMetrics(t *testing.T) {
	s, err := CollectLocalHostMetrics(context.Background())
	if err != nil {
		t.Fatalf("CollectLocalHostMetrics failed: %v", err)
	}
	if s.CPUPercent < 0 || s.CPUPercent > 100 {
		t.Fatalf("cpu out of range: %v", s.CPUPercent)
	}
	if s.MemoryPercent <= 0 || s.MemoryPercent > 100 {
		t.Fatalf("expected real positive memory usage, got %v", s.MemoryPercent)
	}
	if s.DiskPercent < 0 || s.DiskPercent > 100 {
		t.Fatalf("disk out of range: %v", s.DiskPercent)
	}
}
