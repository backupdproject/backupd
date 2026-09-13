package e2eproduction_test

import "syscall"

// peakRSSBytes is this process's high-water resident set, read from the
// kernel rather than from the Go runtime.
//
// It is reported and never asserted on, and the distinction matters: it
// is a high-water mark for the whole test binary, so by the time the
// third scale shape runs it carries the first two's peaks as well. The
// assertions live on the sampled Go heap, which can be attributed to one
// run; this number is here because it is the one an operator's `ps`
// output agrees with, and the one docs/perf records.
func peakRSSBytes() int64 {
	var usage syscall.Rusage

	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}

	return rssBytes(int64(usage.Maxrss))
}
