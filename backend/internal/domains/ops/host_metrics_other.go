//go:build !linux

package ops

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"time"
)

// CollectLocalHostMetrics samples real CPU, memory, and disk utilization on non-Linux dev hosts (Windows/macOS).
func CollectLocalHostMetrics(ctx context.Context) (HostMetricsSample, error) {
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		ps := `$c=(Get-CimInstance Win32_Processor | Measure-Object -Property LoadPercentage -Average).Average; ` +
			`$o=Get-CimInstance Win32_OperatingSystem; ` +
			`$m=if($o.TotalVisibleMemorySize -gt 0){(($o.TotalVisibleMemorySize-$o.FreePhysicalMemory)/$o.TotalVisibleMemorySize)*100}else{0}; ` +
			`$d=Get-CimInstance Win32_LogicalDisk -Filter "DeviceID='C:'"; ` +
			`$dp=if($d -and $d.Size -gt 0){(($d.Size-$d.FreeSpace)/$d.Size)*100}else{0}; ` +
			`Write-Output ("{0:F1} {1:F1} {2:F1}" -f [double]$c, [double]$m, [double]$dp)`
		cmd = exec.CommandContext(runCtx, "powershell", "-NoProfile", "-NonInteractive", "-Command", ps)
	case "darwin":
		sh := `cpu=$(top -l 1 -n 0 | awk '/CPU usage:/{gsub(/%/,"",$3); gsub(/%/,"",$5); printf "%.1f", $3+$5}'); ` +
			`mem=$(vm_stat | awk '/Pages active|Pages wired/{gsub(/\./,"",$NF); u+=$NF} /Pages free|Pages inactive|Pages speculative/{gsub(/\./,"",$NF); f+=$NF} END{if(u+f>0) printf "%.1f", (u/(u+f))*100; else print "0.0"}'); ` +
			`disk=$(df -P / | awk 'NR==2{gsub(/%/,"",$5); printf "%.1f", $5+0}'); ` +
			`printf '%s %s %s\n' "${cpu:-0.0}" "${mem:-0.0}" "${disk:-0.0}"`
		cmd = exec.CommandContext(runCtx, "sh", "-c", sh)
	default:
		return HostMetricsSample{}, errors.New("local host metrics unsupported on " + runtime.GOOS)
	}

	out, err := cmd.Output()
	if err != nil {
		return HostMetricsSample{}, err
	}
	return ParseMetricsTriplet(string(out))
}
