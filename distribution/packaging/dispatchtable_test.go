package packaging

import (
	"regexp"
	"sort"
	"testing"
)

// This file holds the two extractors site_reference_test.go needs to hold
// docs/site/reference.html's command table to main.go's dispatch table.
// They used to live in readme_claims_test.go beside an equivalent check for
// README.md's own command table, which no longer exists: README.md stopped
// carrying a generated command table, and the one promise worth keeping by
// construction is the site's, since that page is where the surface is
// actually documented now.

var (
	commandsMapBlock = regexp.MustCompile(`(?s)var commands = map\[string\]func\(\[\]string\) int\{(.*?)\n\}`)
	commandsMapKey   = regexp.MustCompile(`(?m)^\s*"([a-z-]+)":`)
)

// registeredCommands reads the dispatch table in main.go: the one place
// that decides what `backup-manager <cmd>` actually accepts.
func registeredCommands(t *testing.T, src string) []string {
	t.Helper()
	block := commandsMapBlock.FindStringSubmatch(src)
	if block == nil {
		t.Fatal("could not find the `var commands = map[string]func([]string) int{...}` dispatch table in main.go; site_reference_test.go reads it to decide what the CLI accepts")
	}
	var out []string
	for _, m := range commandsMapKey.FindAllStringSubmatch(block[1], -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

func diffSets(want, got []string) (missing, extra []string) {
	w := map[string]bool{}
	for _, s := range want {
		w[s] = true
	}
	g := map[string]bool{}
	for _, s := range got {
		g[s] = true
	}
	for _, s := range want {
		if !g[s] {
			missing = append(missing, s)
		}
	}
	for _, s := range got {
		if !w[s] {
			extra = append(extra, s)
		}
	}
	return missing, extra
}
