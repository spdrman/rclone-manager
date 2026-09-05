// One helper, kept out of the capture files so the CLI runner reads as a
// single flow rather than breaking mid-line to declare a target variable
// for errors.As.
package compat

import (
	"errors"
	"os/exec"
)

// asExitError is errors.As with the one type this package cares about,
// pulled out so runCLI reads as a single flow.
func asExitError(err error, target **exec.ExitError) bool {
	return errors.As(err, target)
}
