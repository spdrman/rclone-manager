//go:build !race

package rclone

// raceDetectorEnabled is false in an ordinary `go test` build. See
// racebuild_race_test.go for the one thing that reads it.
const raceDetectorEnabled = false
