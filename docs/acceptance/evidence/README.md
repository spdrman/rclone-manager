# Hardware evidence records

One file per architecture, produced on a real device by
[`../ugos-resource-certification.md`](../ugos-resource-certification.md)
and committed alongside the claim it backs.

```
ugos-resource-amd64.json
ugos-resource-arm64.json
```

An architecture with no file here is **uncertified**, and that is a real
state rather than a gap in this directory: §68's rule is that a provider or
architecture is build-supported and uncertified until its acceptance test is
completed. `go run ./cmd/hwcert status`, from `distribution/`, reads this
directory and says which is which.

## What is in one

Everything §68 asks a certification record to carry, plus the raw samples
the aggregates were computed from, so a later reader can re-derive every
number rather than taking the harness's word for it: architecture, the
probe's own `GOARCH` and the device's `uname -m`, provider, vendor, model,
UGOS firmware version, kernel, cores, memory, the release under test, the
deployment as it was actually running, the sampling method, and the
measurements.

## What is not in one

No credential, no token, no device address. The measurement driver reads
the administrator password from a terminal prompt and hands it to `curl`
down a pipe; nothing it writes carries anything but measurements and
device identity, which is what makes these files safe to commit.

## Why one file cannot cover two architectures

Three independent things say what architecture a record is for: the
operator's declaration, the architecture the probe binary was compiled
for, and the machine the kernel reports. `distribution/hwcert` refuses a
record where any two of them disagree, and refuses to read a record as
evidence for an architecture other than its own. So copying an amd64 run
over the arm64 file does not produce an arm64 pass, it produces a refusal.

## Re-running

A record is replaced by a new run, never edited. If a number in one is
wrong, the fix is another run and another commit, because the value of
these files is that each one is a thing that happened on a day, on a
device, at a firmware version, all of which the file names.
