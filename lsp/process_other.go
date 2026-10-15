//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly

package lsp

import "os/exec"

func configureProcess(_ *exec.Cmd) func() { return func() {} }
