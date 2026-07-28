//go:build windows

package main

import "os/exec"

func configureProcess(*exec.Cmd) {}

func killProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
