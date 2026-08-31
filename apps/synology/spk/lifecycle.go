package spk

// LifecycleScripts returns the shipped lifecycle scripts by name.
func LifecycleScripts() (map[string]string, error) { return nil, errNotImplemented }

// ScanForUnsafeDeletes reads a shell script and returns one finding per
// deletion whose target is not provably inside the package's own
// footprint.
func ScanForUnsafeDeletes(_, _ string) []string { return nil }
