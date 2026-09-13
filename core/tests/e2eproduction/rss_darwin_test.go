//go:build darwin

package e2eproduction_test

// rssBytes converts this platform's ru_maxrss into bytes. Darwin reports
// it in bytes already, which is the whole reason this is two files: the
// same number means two different things on the two platforms this
// product is built for, and a single conversion would be wrong on one of
// them by a factor of 1024.
func rssBytes(maxrss int64) int64 { return maxrss }
