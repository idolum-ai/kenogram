//go:build !darwin

package backend

func stdinIsTerminal() bool { return false }
