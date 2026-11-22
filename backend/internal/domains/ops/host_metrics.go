package ops

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/executor"
	"github.com/jackc/pgx/v5/pgxpool"
)

// HostMetricsSample represents a point-in-time CPU, memory, and root disk utilization snapshot (0..100).
type HostMetricsSample struct {
	CPUPercent    float64   `json:"cpu_percent"`
	MemoryPercent float64   `json:"memory_percent"`
	DiskPercent   float64   `json:"disk_percent"`
	RecordedAt    time.Time `json:"recorded_at"`
}

func clampPercent(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return math.Round(v*10) / 10
}

// parseProcStatCPU parses the aggregate "cpu ..." line from Linux /proc/stat into (idle, total) ticks.
func parseProcStatCPU(content string) (idle uint64, total uint64, ok bool) {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			return 0, 0, false
		}
		var vals []uint64
		for _, f := range fields[1:] {
			n, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return 0, 0, false
			}
			vals = append(vals, n)
			total += n
		}
		idle = vals[3]
		if len(vals) > 4 {
			idle += vals[4] // iowait
		}
		return idle, total, true
	}
	return 0, 0, false
}

// parseProcMeminfo parses Linux /proc/meminfo into memory usage percentage (0..100).
func parseProcMeminfo(content string) (float64, bool) {
	var memTotal, memAvailable, memFree, buffers, cached uint64
	var hasAvail bool
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		val, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			memTotal = val
		case "MemAvailable:":
			memAvailable = val
			hasAvail = true
		case "MemFree:":
			memFree = val
		case "Buffers:":
			buffers = val
		case "Cached:":
			cached = val
		}
	}
	if memTotal == 0 {
		return 0, false
	}
	avail := memAvailable
	if !hasAvail {
		avail = memFree + buffers + cached
	}
	if avail > memTotal {
		avail = memTotal
	}
	used := float64(memTotal - avail)
	return clampPercent((used / float64(memTotal)) * 100.0), true
}

// ParseMetricsTriplet parses a whitespace-separated "cpu mem disk" line (e.g. "12.4 45.2 31.0").
func ParseMetricsTriplet(stdout string) (HostMetricsSample, error) {
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		fields := strings.Fields(strings.TrimSpace(lines[i]))
		if len(fields) < 3 {
			continue
		}
		cpu, err1 := strconv.ParseFloat(strings.TrimSuffix(fields[0], "%"), 64)
		mem, err2 := strconv.ParseFloat(strings.TrimSuffix(fields[1], "%"), 64)
		disk, err3 := strconv.ParseFloat(strings.TrimSuffix(fields[2], "%"), 64)
		if err1 == nil && err2 == nil && err3 == nil {
			return HostMetricsSample{
				CPUPercent:    clampPercent(cpu),
				MemoryPercent: clampPercent(mem),
				DiskPercent:   clampPercent(disk),
				RecordedAt:    time.Now().UTC(),
			}, nil
		}
	}
	return HostMetricsSample{}, errors.New("could not parse host metrics output")
}

// remoteLinuxMetricsScript is a single dependency-free POSIX sh snippet that samples
// CPU (over 200ms via /proc/stat), Memory (/proc/meminfo), and root Disk (df -P /).
const remoteLinuxMetricsScript = `read _ u1 n1 s1 i1 w1 q1 sq1 _ < /proc/stat
sleep 0.2
read _ u2 n2 s2 i2 w2 q2 sq2 _ < /proc/stat
t1=$((u1+n1+s1+i1+w1+q1+sq1))
t2=$((u2+n2+s2+i2+w2+q2+sq2))
id1=$((i1+w1))
id2=$((i2+w2))
cpu=$(awk -v dt=$((t2-t1)) -v di=$((id2-id1)) 'BEGIN { if (dt>0) printf "%.1f", ((dt-di)/dt)*100; else print "0.0" }')
mem=$(awk '/^MemTotal:/{t=$2} /^MemAvailable:/{a=$2;ha=1} /^MemFree:/{f=$2} /^Buffers:/{b=$2} /^Cached:/{c=$2} END{if(!ha)a=f+b+c; if(t>0) printf "%.1f", ((t-a)/t)*100; else print "0.0"}' /proc/meminfo)
disk=$(df -P / | awk 'NR==2{gsub(/%/,"",$5); printf "%.1f", $5+0}')
printf '%s %s %s\n' "$cpu" "$mem" "$disk"`

// CollectRemoteServerMetrics executes a lightweight read-only shell probe over SSH to sample CPU, memory, and disk.
func CollectRemoteServerMetrics(
	ctx context.Context,
	execClient *executor.Client,
	serverUUID string,
	teamID int64,
	host, user string,
	port int,
	sshKey string,
) (HostMetricsSample, error) {
	if execClient == nil {
		return HostMetricsSample{}, errors.New("executor client is nil")
	}
	if port <= 0 {
		port = 22
	}
	if strings.TrimSpace(user) == "" {
		user = "root"
	}
	var out strings.Builder
	opID := fmt.Sprintf("metrics-%s-%d", serverUUID, time.Now().UnixNano())
	res, err := execClient.Run(ctx, executor.ExecutionRequest{
		ProtocolVersion:    executor.ProtocolVersionV1,
		FrameType:          executor.FrameStart,
		JobID:              opID,
		OperationID:        opID,
		AttemptID:          "1",
		OwnershipEpoch:     1,
		ResourceUUID:       serverUUID,
		ExpectedCapability: "ssh_exec",
		TeamID:             teamID,
		DeadlineMS:         10000,
		Operation: executor.OperationSpec{
			Kind:          "exec",
			Host:          host,
			Port:          port,
			User:          user,
			SSHKeyContent: sshKey,
			Command:       remoteLinuxMetricsScript,
		},
	}, func(ev executor.ExecutionEvent) error {
		if ev.EventType == "output" && (ev.Stream == "stdout" || ev.Stream == "") {
			out.WriteString(ev.Data)
			out.WriteByte('\n')
		}
		return nil
	})
	if err != nil {
		return HostMetricsSample{}, err
	}
	if res.ExitCode != 0 {
		return HostMetricsSample{}, fmt.Errorf("remote metrics probe exited with code %d", res.ExitCode)
	}
	return ParseMetricsTriplet(out.String())
}

// RecordServerMetrics persists a HostMetricsSample into go_server_metrics.
func RecordServerMetrics(ctx context.Context, pool *pgxpool.Pool, serverID int64, sample HostMetricsSample) error {
	if pool == nil {
		return errors.New("postgres pool is nil")
	}
	recordedAt := sample.RecordedAt
	if recordedAt.IsZero() {
		recordedAt = time.Now().UTC()
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO go_server_metrics (server_id, cpu_percent, memory_percent, disk_percent, recorded_at)
		VALUES ($1, $2, $3, $4, $5)
	`, serverID, clampPercent(sample.CPUPercent), clampPercent(sample.MemoryPercent), clampPercent(sample.DiskPercent), recordedAt)
	return err
}

// PruneOldServerMetrics removes metric rows older than maxAge so go_server_metrics stays bounded.
func PruneOldServerMetrics(ctx context.Context, pool *pgxpool.Pool, maxAge time.Duration) (int64, error) {
	if pool == nil {
		return 0, nil
	}
	if maxAge <= 0 {
		maxAge = 30 * 24 * time.Hour
	}
	cutoff := time.Now().UTC().Add(-maxAge)
	tag, err := pool.Exec(ctx, `DELETE FROM go_server_metrics WHERE recorded_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
