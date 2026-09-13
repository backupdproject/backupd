//go:build !darwin

package e2eproduction_test

// rssBytes converts this platform's ru_maxrss into bytes. Linux, and
// every other platform this product targets, reports it in kilobytes.
func rssBytes(maxrss int64) int64 { return maxrss * 1024 }
