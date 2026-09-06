package machines

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// The one place this package shells out to docker.
//
// It is a file with two functions in it because the property that matters is
// that there is no second route. Every docker call the tier makes is bounded
// by a timeout and reports a timeout differently from a non-zero exit, and
// both of those hold only for as long as nothing calls exec.Command directly.
// #161 is what that costs when it is not true: an unbounded call outran every
// deadline the retry loops around it thought they had, and the package hung
// to its go test timeout with no indication of which call was stuck.

// dockerRun is the only place this package shells out to docker, and every
// call through it is bounded. Before #161 each one was a plain
// exec.Command with no timeout, which is why the retry loops elsewhere in
// this package could not do what their deadlines promised: they only
// re-read the deadline between attempts, so a single call that never
// returned outran all of them and took the whole package to its go test
// timeout.
//
// A timeout and a non-zero exit are told apart, because they mean
// completely different things: one is a statement about the daemon, the
// other about the container.
func dockerRun(timeout time.Duration, args ...string) (stdout, stderr string, err error) {
	return dockerRunStdin(timeout, "", args...)
}

// dockerRunStdin is dockerRun with something on the command's stdin, which
// is how a Dockerfile reaches `docker build -` without a directory on disk.
func dockerRunStdin(timeout time.Duration, stdin string, args ...string) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	stdout = strings.TrimSpace(outBuf.String())
	stderr = strings.TrimSpace(errBuf.String())

	switch {
	case ctx.Err() != nil:
		return stdout, stderr, fmt.Errorf("%w: `docker %s` was still running after %s", errDockerTimedOut, args[0], timeout)
	case runErr != nil:
		return stdout, stderr, fmt.Errorf("%w: %s", runErr, stderr)
	}
	return stdout, stderr, nil
}
