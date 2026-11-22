package controlplane

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func TestRegistryPasswordPipelineClosesWithoutClosingSSHStdin(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil && runtime.GOOS == "windows" {
		candidate := `C:\Program Files\Git\bin\bash.exe`
		if _, statErr := os.Stat(candidate); statErr == nil {
			shell = candidate
			err = nil
		}
	}
	if err != nil {
		t.Skip("POSIX shell unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// This Docker substitute reads until EOF just like password-stdin. Parent
	// stdin deliberately stays open, as it does during a Rust executor request.
	command := `docker() { [ "$1" = login ] && [ "$2" = --username ] && [ "$3" = "user'\$(echo injected)" ] && [ "$4" = --password-stdin ] && [ "$5" = ghcr.io ] || return 2; cat; }; ` + registryLoginCommand("ghcr.io", "user'$(echo injected)")
	cmd := exec.CommandContext(ctx, shell, "-c", command)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	password := "quoted'password\nwith a second line"
	if _, err := stdin.Write([]byte(registryPasswordInput(password))); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("registry pipeline failed or waited for SSH EOF: %v, %s", err, output.String())
	}
	if output.String() != password {
		t.Fatal("password bytes changed or shell substitution executed")
	}
}
