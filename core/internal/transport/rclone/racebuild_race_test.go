//go:build race

package rclone

// raceDetectorEnabled is true in a `go test -race` build.
//
// The one thing in this package that has to know is
// TestDisableSignalExit: it observes the exit status of a child process,
// and the detector's own exit status (66) is a different claim about the
// same number. See signalexit_test.go's childGORACE.
const raceDetectorEnabled = true
