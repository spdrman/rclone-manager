// Package hwcert is the measurement harness behind D2.1's resource and
// hardware certification on real UGREEN devices (issue #89, EPIC D #177,
// originally §73 Work Package 5.3 of docs/EPIC-B-multi-nas.md).
//
// Idle CPU, resident memory and transfer overhead on a physical NAS are
// not things a unit test can assert. What a unit test can assert is
// everything that decides whether a set of numbers is a pass, and that is
// what lives here: the aggregation, the threshold comparison, the
// architecture guard, the evidence record and the shape checks. The
// sampling itself is scripts/hwcert/measure-ugos.sh, which runs on the
// device and decides nothing.
//
// # The thresholds are not in this package
//
// #89's acceptance criterion says the thresholds are documented in the
// acceptance procedure before hardware execution rather than decided after
// seeing the numbers. So they live in
// docs/acceptance/ugos-resource-certification.md, in fenced blocks this
// package parses, and there is no copy of any of them in Go. Tuning one to
// fit a result means editing that document, in a diff, in review. The same
// goes for the sampling method and for the formula behind each derived
// ratio.
//
// # Nothing here reads docs/perf
//
// #165's baselines were captured on a designated benchmark host. UGREEN
// hardware is not that host, and comparing across machines reports the
// machine rather than the change, so a UGOS record is its own evidence and
// is never compared to a docs/perf baseline. What does carry across hosts
// is the shape of the claim, which is why CheckShape exists: no data-path
// hop added by the adapter, no sidecar, no second application server. Those
// are properties of the deployment, decidable on any device.
//
// # An amd64 pass never implies an arm64 pass
//
// A record carries three independent witnesses to its architecture: the
// one the operator declared, the GOARCH the probe binary was compiled for,
// and the machine the kernel reports. Validate refuses a record where any
// two disagree, so an amd64 probe cannot produce an arm64 record, and
// Verify refuses a record whose architecture is not the one being
// verified. Status reports an architecture with no record as uncertified
// rather than leaving the question open.
package hwcert
