//go:build !linux && !darwin

package lockfile

func ProcessStart(int) string { return "" }
