//go:build !linux

package review

import "os/exec"

func setupProcessGroup(cmd *exec.Cmd) {}
