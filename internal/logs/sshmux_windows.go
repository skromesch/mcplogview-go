//go:build windows

package logs

// prepareMuxDir never multiplexes on Windows.
func prepareMuxDir(string) string { return "" }
