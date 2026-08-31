package packaging

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode"
)

// This file is issue #170's own suite: the four targets Phase 6 adds, and
// the properties that make each of them an adapter rather than an
// application.
//
// None of the four is a conversion. #169 converted the five platforms
// Phase 4 shipped, and it was written as a conversion because there was
// packaging to convert. Portainer, Dockge, CasaOS and ZimaOS have no
// Phase 4 counterpart at all: no issue in this repository targeted a
// container manager or a third-party app store before this one, so
// nothing below replaces anything and there is no migration path to
// preserve.
//
// The shapes are constrained rather than chosen. EPIC B #81's exit gate
// says Portainer support is template or stack based and never a product
// plugin, Dockge uses the canonical Compose stack, and CasaOS and ZimaOS
// reuse Compose and container semantics. Every test here is one of those
// sentences with a check behind it, and every negative claim has a
// control that watches it fail against a fixture this test built, never
// against the real tree.

// newAdapters are the four targets #170 adds, and how to read each one.
type newAdapter struct {
	// id is the apps/<id>/ directory and the canonical.json platform key.
	id string
	// compose is the adapter's own runtime definition, relative to
	// apps/<id>/, or empty for an adapter that ships none because it
	// deploys the canonical stack itself.
	compose string
	// env is the file compose is read with, relative to apps/<id>/, or
	// empty for a store format that deploys its file as it stands.
	env string
	// acceptance is the §68 procedure, relative to the repository root.
	acceptance string
}

func newAdapters() []newAdapter {
	return []newAdapter{
		{id: "portainer", compose: "compose/backup-manager.yml", env: "compose/backup-manager.env", acceptance: "docs/acceptance/portainer-stack-deployment.md"},
		{id: "dockge", acceptance: "docs/acceptance/dockge-stack-import.md"},
		{id: "casaos", compose: "compose/backup-manager.yml", acceptance: "docs/acceptance/casaos-app-store-install.md"},
		{id: "zimaos", compose: "compose/backup-manager.yml", acceptance: "docs/acceptance/zimaos-app-store-install.md"},
	}
}

// services reads whichever runtime definition this adapter actually
// deploys: its own, or the canonical stack for the one that ships none.
func (a newAdapter) services(t *testing.T) []Service {
	t.Helper()
	if a.compose == "" {
		return canonicalStackServices(t)
	}
	var env map[string]string
	if a.env != "" {
		var err error
		env, err = ReadEnvFile(filepath.Join(PlatformDir(a.id), a.env))
		if err != nil {
			t.Fatalf("%s: read env file: %v", a.id, err)
		}
	}
	svcs, err := ReadCompose(filepath.Join(PlatformDir(a.id), a.compose), env)
	if err != nil {
		t.Fatalf("%s: read compose: %v", a.id, err)
	}
	return svcs
}

// envFile is the environment this adapter's stack is read with, which is
// the canonical example environment for the one that deploys the
// canonical stack.
func (a newAdapter) envFile(t *testing.T) map[string]string {
	t.Helper()
	path := filepath.Join(RepoRoot, "container", ".env.example")
	if a.compose != "" {
		if a.env == "" {
			return nil
		}
		path = filepath.Join(PlatformDir(a.id), a.env)
	}
	env, err := ReadEnvFile(path)
	if err != nil {
		t.Fatalf("%s: read env file: %v", a.id, err)
	}
	return env
}

// canonicalStackServices is container/compose.yaml read with its own
// example environment, which is the runtime every one of these four is
// compared against and the one Dockge deploys directly.
func canonicalStackServices(t *testing.T) []Service {
	t.Helper()
	env, err := ReadEnvFile(filepath.Join(RepoRoot, "container", ".env.example"))
	if err != nil {
		t.Fatalf("read container/.env.example: %v", err)
	}
	svcs, err := ReadCompose(filepath.Join(RepoRoot, "container", "compose.yaml"), env)
	if err != nil {
		t.Fatalf("read container/compose.yaml: %v", err)
	}
	return svcs
}

func canonicalRuntime(t *testing.T, c Canonical) AdapterRuntime {
	t.Helper()
	rt, drift := ReduceToRoles("generic", canonicalStackServices(t), c)
	if len(drift) > 0 {
		t.Fatalf("the canonical stack itself did not reduce to two roles, so every comparison in this file would be against nothing:\n%s", FormatDrift(drift))
	}
	if rt.Engine == nil || rt.WebUI == nil {
		t.Fatal("the canonical stack reduced to fewer than two roles")
	}
	return rt
}

// ---------------------------------------------------------------------
// Semantic equivalence with the canonical stack
// ---------------------------------------------------------------------

func TestEveryNewAdapterIsSemanticallyEquivalentToTheCanonicalStack(t *testing.T) {
	c := MustLoad()
	want := canonicalRuntime(t, c)

	for _, a := range newAdapters() {
		t.Run(a.id, func(t *testing.T) {
			got, drift := ReduceToRoles(a.id, a.services(t), c)
			if len(drift) > 0 {
				t.Fatalf("could not reduce this adapter to roles:\n%s", FormatDrift(drift))
			}
			if d := CheckStackEquivalence(got, want); len(d) > 0 {
				t.Errorf("this adapter's runtime is not the canonical runtime:\n%s", FormatDivergence(d))
			}
		})
	}
}

// mutateForEquivalence returns a DEEP COPY of a runtime with exactly one
// equivalence property broken, and says what it broke.
//
// The copy is the point rather than a detail. Every service in this
// package is read out of a file into a fresh struct, but the slices and
// maps inside it are shared with whatever else holds that Service, and a
// control that mutates shared state is not a control: it changes the
// answer for the tests that run after it and its own result stops meaning
// anything.
func mutateForEquivalence(a AdapterRuntime, property string) (AdapterRuntime, string, bool) {
	out := AdapterRuntime{Platform: a.Platform}
	if a.Engine != nil {
		e := copyService(*a.Engine)
		out.Engine = &e
	}
	if a.WebUI != nil {
		w := copyService(*a.WebUI)
		out.WebUI = &w
	}
	out.Others = append([]Service(nil), a.Others...)

	switch property {
	case PropRoleSet:
		out.Others = append(out.Others, Service{Name: "backup-manager-sidecar", Command: []string{"/backup-manager", "daemon"}})
		return out, "added a third container", true
	case PropCommand:
		if out.WebUI == nil {
			return out, "", false
		}
		out.WebUI.Command = append([]string{"/backup-manager-web", "serve"}, out.WebUI.Command[2:]...)
		return out, "made the Web UI run the engine command", true
	case PropContainerMounts:
		if out.Engine == nil || len(out.Engine.Mounts) == 0 {
			return out, "", false
		}
		out.Engine.Mounts[0].ContainerPath = "/data/somewhere-else"
		return out, "moved the engine's first mount to a container path the binaries do not read", true
	case PropPublishedPort:
		if out.WebUI == nil {
			return out, "", false
		}
		out.WebUI.Ports = []string{"8080:9090"}
		return out, "published a container port nothing listens on", true
	case PropHealthCheck:
		if out.WebUI == nil {
			return out, "", false
		}
		out.WebUI.HealthcheckTest = []string{"CMD", "/backup-manager", "status"}
		return out, "gave the Web UI the engine's health check, which needs a state database it does not have", true
	case PropEngineEnvironment:
		if out.Engine == nil {
			return out, "", false
		}
		delete(out.Engine.Environment, "TZ")
		return out, "stopped the engine declaring TZ, so retention boundaries fall back to the image's UTC default", true
	}
	return out, "", false
}

func copyService(s Service) Service {
	out := s
	out.Command = append([]string(nil), s.Command...)
	out.Mounts = append([]Mount(nil), s.Mounts...)
	out.Ports = append([]string(nil), s.Ports...)
	out.HealthcheckTest = append([]string(nil), s.HealthcheckTest...)
	out.Environment = map[string]string{}
	for k, v := range s.Environment {
		out.Environment[k] = v
	}
	return out
}

// TestTheEquivalenceCheckFailsOnADeliberateMismatch is the control, and
// it runs every property against every one of the four rather than
// against a single fixture: three of them are read out of a compose file
// with an env file, one out of a compose file with none, and one is the
// canonical stack itself, so a property that fires on one shape and not
// on another is a property those adapters do not have.
func TestTheEquivalenceCheckFailsOnADeliberateMismatch(t *testing.T) {
	c := MustLoad()
	want := canonicalRuntime(t, c)

	for _, a := range newAdapters() {
		base, drift := ReduceToRoles(a.id, a.services(t), c)
		if len(drift) > 0 {
			t.Fatalf("%s: could not reduce to roles: %s", a.id, FormatDrift(drift))
		}
		if d := CheckStackEquivalence(base, want); len(d) > 0 {
			t.Fatalf("%s already diverges before any mutation, so no control below proves anything:\n%s", a.id, FormatDivergence(d))
		}

		for _, property := range EquivalenceProperties {
			t.Run(a.id+"/"+property.ID, func(t *testing.T) {
				mutated, what, ok := mutateForEquivalence(base, property.ID)
				if !ok {
					t.Fatalf("no mutation is defined for %q, so nobody has watched that comparison fail", property.ID)
				}
				found := CheckStackEquivalence(mutated, want)
				if len(found) == 0 {
					t.Fatalf("%s and the equivalence check still passed", what)
				}
				named := false
				for _, d := range found {
					if d.Property == property.ID {
						named = true
					}
				}
				if !named {
					t.Errorf("%s and the check failed for a different reason, so this control does not exercise %q:\n%s", what, property.ID, FormatDivergence(found))
				}

				// And the mutation stayed inside the copy: the real
				// adapter still passes afterwards.
				if d := CheckStackEquivalence(base, want); len(d) > 0 {
					t.Errorf("the mutation reached the shared runtime, so every later comparison is against a broken adapter:\n%s", FormatDivergence(d))
				}
			})
		}
	}
}

// TestEveryEquivalencePropertyIsCompared pins the exported property list
// to what the checker actually looks at. A comparison whose list and
// whose implementation disagree has a hole that is invisible from either
// side, which is the same failure DerivedFields guards against for the
// derivation gate.
func TestEveryEquivalencePropertyIsCompared(t *testing.T) {
	c := MustLoad()
	want := canonicalRuntime(t, c)
	base, _ := ReduceToRoles("portainer", newAdapters()[0].services(t), c)

	seen := map[string]bool{}
	for _, property := range EquivalenceProperties {
		mutated, _, ok := mutateForEquivalence(base, property.ID)
		if !ok {
			t.Errorf("EquivalenceProperties declares %q and no mutation breaks it", property.ID)
			continue
		}
		for _, d := range CheckStackEquivalence(mutated, want) {
			seen[d.Property] = true
		}
		if strings.TrimSpace(property.Why) == "" {
			t.Errorf("%q has no stated reason; a comparison nobody can justify is one that gets relaxed", property.ID)
		}
	}
	for id := range seen {
		found := false
		for _, property := range EquivalenceProperties {
			if property.ID == id {
				found = true
			}
		}
		if !found {
			t.Errorf("the checker reports property %q, which EquivalenceProperties does not declare", id)
		}
	}
}

// ---------------------------------------------------------------------
// Runtime profile and UI bundle
// ---------------------------------------------------------------------

// TestEveryNewAdapterSelectsTheGenericProfileAndServesTheEmbeddedBundle
// is the decision the four share, and the one a reader is most likely to
// think is an oversight.
//
// They select `generic` because none of them has host-dependent behaviour
// to select: no native identity gateway, no notification bridge, no
// launch bridge, no capability to report. A profile per platform that
// changed nothing would be four rows in the runtime's profile table and
// four platform ids in the /api/v1 contract, the capability table and the
// bundle list, which is core and shared-UI code inside adapters whose own
// contract forbids it.
//
// They serve the bundle compiled into the binary for a reason that is
// measured rather than argued: the canonical image carries five
// per-provider bundles and has 347,956 bytes of headroom against its
// gated 5% ceiling, and one bundle is roughly 352 KB, so it cannot carry
// a sixth, let alone four more. Setting UI_ROOT here would not degrade
// quietly either: serve-ui refuses to start when the selected bundle is
// missing, so an adapter that pointed at one would be an adapter that
// does not come up.
func TestEveryNewAdapterSelectsTheGenericProfileAndServesTheEmbeddedBundle(t *testing.T) {
	c := MustLoad()

	for _, a := range newAdapters() {
		t.Run(a.id, func(t *testing.T) {
			platform, ok := c.Platforms[a.id]
			if !ok {
				t.Fatalf("canonical.json declares no platform %q, so nothing derives this adapter", a.id)
			}
			if platform.Profile != "generic" {
				t.Errorf("canonical.json gives this platform profile %q; none of these four has host-dependent behaviour to select, and a profile that changes nothing is a platform id in the core", platform.Profile)
			}
			if platform.UIBridge != UIBridgeNone {
				t.Errorf("canonical.json declares uiBridge %q; a bundle for this platform cannot be carried anywhere (see the image-size headroom in docs/runtime-contract.md)", platform.UIBridge)
			}

			rt, drift := ReduceToRoles(a.id, a.services(t), c)
			if len(drift) > 0 {
				t.Fatalf("could not reduce this adapter to roles:\n%s", FormatDrift(drift))
			}
			env := a.envFile(t)
			for _, svc := range []*Service{rt.Engine, rt.WebUI} {
				if svc == nil {
					t.Fatal("this adapter does not declare both roles")
				}
				// The command is compared AFTER variable expansion.
				// ParseCompose expands the image, the environment, the
				// ports and the volumes and leaves `command` alone, and
				// the canonical stack writes its profile as
				// ${RUNTIME_PROFILE:-generic}, so a literal comparison
				// here would report the one adapter that deploys the
				// canonical stack as selecting no profile at all.
				selected := ""
				for _, arg := range svc.Command {
					if !strings.HasPrefix(arg, profileArg) {
						continue
					}
					selected, _ = ExpandCompose(strings.TrimPrefix(arg, profileArg), env)
				}
				if selected != platform.Profile {
					t.Errorf("service %q runs %v, which selects profile %q and canonical.json gives this platform %q", svc.Name, svc.Command, selected, platform.Profile)
				}
			}

			sel := SelectUIBundle(rt.WebUI, UIBundleSelection{Mechanism: UIBundleNone}, platform.Profile)
			if sel.Mechanism != UIBundleEmbedded {
				t.Errorf("the Web UI service selects a %q bundle (%s); this adapter has no carrier for one, and serve-ui refuses to start when a selected bundle is missing rather than falling back",
					sel.Mechanism, sel.Detail)
			}
			shipped, source, err := ShippedBridgeProvider()
			if err != nil {
				t.Fatal(err)
			}
			if shipped != "generic" {
				t.Errorf("the bundle compiled into the binary is %s's, per %s, and these adapters serve that bundle: they would show a NAS provider's shell on a plain Docker host", shipped, source)
			}
		})
	}
}

// TestNoNewAdapterWiresAuthenticationOfItsOwn is what the four columns'
// auth-mode-explicit cells point at, and it is here rather than left as a
// declaration for exactly that reason: the matrix cell records that the
// bridge-shaped half of the check does not apply, and the half that is
// about security has to be checked somewhere rather than excused.
func TestNoNewAdapterWiresAuthenticationOfItsOwn(t *testing.T) {
	c := MustLoad()

	for _, a := range newAdapters() {
		t.Run(a.id, func(t *testing.T) {
			root := PlatformDir(a.id)
			files, err := os.ReadDir(root)
			if err != nil {
				t.Fatalf("read %s: %v", root, err)
			}
			if len(files) == 0 {
				t.Fatal("this adapter's directory is empty, so a clean scan below would prove nothing")
			}
			findings, err := ScanForBespokeAuth(root)
			if err != nil {
				t.Fatalf("ScanForBespokeAuth(%s): %v", root, err)
			}
			if len(findings) > 0 {
				t.Errorf("%s wires an authentication mechanism of its own:\n%s", root, format(findings))
			}

			// The other half: the runtime it deploys is the canonical
			// one, whose auth mode is the canonical one. Reading it off
			// the profile rather than off a bridge is the whole point.
			if c.AuthMode != "local-account" {
				t.Fatalf("canonical.json declares auth mode %q; this test asserts these adapters inherit it and there is nothing to inherit", c.AuthMode)
			}
			if got := c.Platforms[a.id].Profile; got != "generic" {
				t.Errorf("this adapter selects profile %q, so it does not inherit the generic profile's local authentication", got)
			}
		})
	}
}

// ---------------------------------------------------------------------
// Portainer: a template, never a plugin
// ---------------------------------------------------------------------

func TestThePortainerTemplateIsAStackTemplateAndNotAPlugin(t *testing.T) {
	const (
		root      = "apps/portainer"
		stackfile = "apps/portainer/compose/backup-manager.yml"
	)
	tpl, err := ReadPortainerTemplates(Path(filepath.Join(root, "templates.json")))
	if err != nil {
		t.Fatalf("read the App Template: %v", err)
	}
	env, err := ReadEnvFile(Path(filepath.Join(root, "compose", "backup-manager.env")))
	if err != nil {
		t.Fatalf("read the env file: %v", err)
	}
	composeBody, err := os.ReadFile(Path(stackfile))
	if err != nil {
		t.Fatal(err)
	}
	vars := distinctVarNames(string(composeBody))
	if len(vars) == 0 {
		t.Fatal("the stack reads no variables at all, so the template/stack comparison below would pass vacuously")
	}

	if v := CheckPortainerTemplate("templates.json", tpl, vars, env, stackfile); len(v) > 0 {
		t.Errorf("the App Template does not describe the stack it deploys:\n%s", format(v))
	}

	// The socket half is checked structurally rather than textually,
	// because the compose file's own comment says the word and a
	// substring match cannot tell a comment from a mount. This is the
	// security question that matters most for this platform: Portainer
	// has the socket, and this product must not inherit it.
	svcs, err := ReadCompose(Path(stackfile), env)
	if err != nil {
		t.Fatal(err)
	}
	if len(svcs) == 0 {
		t.Fatal("the stack declares no services, so the host-path prohibition below would pass vacuously")
	}
	if v := CheckMountedHostPaths(svcs); len(v) > 0 {
		t.Errorf("the stack mounts a prohibited host path:\n%s", format(v))
	}
	socketed := make([]Service, len(svcs))
	copy(socketed, svcs)
	socketed[0].Mounts = append(append([]Mount(nil), svcs[0].Mounts...),
		Mount{HostPath: "/var/run/docker.sock", ContainerPath: "/var/run/docker.sock"})
	if v := CheckMountedHostPaths(socketed); len(v) == 0 {
		t.Error("a stack that binds the Docker socket passed the prohibition, so the clean result above is a rule that is not running")
	}
}

// TestThePortainerTemplateCheckFailsOnEveryWayItCanBeWrong is the control.
// Every branch of CheckPortainerTemplate is a negative claim about a file
// that currently satisfies all of them, and a rule nobody has watched
// fail is a comment.
func TestThePortainerTemplateCheckFailsOnEveryWayItCanBeWrong(t *testing.T) {
	const stackfile = "apps/portainer/compose/backup-manager.yml"
	good := PortainerTemplates{
		Version: "3",
		Templates: []PortainerTemplate{{
			Type:        3,
			Title:       "Backup Manager",
			Name:        "backup-manager",
			Description: "d",
			Logo:        "https://example.invalid/logo.svg",
			Platform:    "linux",
			Categories:  []string{"backup"},
			Env: []PortainerEnv{
				{Name: "STATE_DIR", Label: "state", Default: "/opt/backup-manager/state"},
			},
		}},
	}
	good.Templates[0].Repository.URL = "https://example.invalid/repo"
	good.Templates[0].Repository.Stackfile = stackfile
	vars := []string{"STATE_DIR"}
	env := map[string]string{"STATE_DIR": "/opt/backup-manager/state"}

	if v := CheckPortainerTemplate("fixture", good, vars, env, stackfile); len(v) > 0 {
		t.Fatalf("the clean fixture already fails, so no control below means anything:\n%s", format(v))
	}

	mutate := func(f func(*PortainerTemplates)) PortainerTemplates {
		out := good
		out.Templates = append([]PortainerTemplate(nil), good.Templates...)
		out.Templates[0].Env = append([]PortainerEnv(nil), good.Templates[0].Env...)
		f(&out)
		return out
	}

	cases := []struct {
		name string
		tpl  PortainerTemplates
		rule string
		want string
	}{
		{"a container template rather than a stack template",
			mutate(func(t *PortainerTemplates) { t.Templates[0].Type = 1 }),
			RulePortainerTemplate, "type 1"},
		{"privileged",
			mutate(func(t *PortainerTemplates) { t.Templates[0].Privileged = true }),
			RulePrivileged, "privileged"},
		{"binds the Docker socket",
			mutate(func(t *PortainerTemplates) {
				t.Templates[0].Volumes = append(t.Templates[0].Volumes, struct {
					Container string `json:"container"`
					Bind      string `json:"bind"`
				}{Container: "/var/run/docker.sock", Bind: "/var/run/docker.sock"})
			}),
			RuleProhibitedHostPath, "docker.sock"},
		{"names an image of its own",
			mutate(func(t *PortainerTemplates) { t.Templates[0].Image = "someone-elses/image:latest" }),
			RulePortainerTemplate, "names an image"},
		{"points at a stack file that is not this adapter's",
			mutate(func(t *PortainerTemplates) { t.Templates[0].Repository.Stackfile = "somewhere/else.yml" }),
			RulePortainerTemplate, "repository.stackfile"},
		{"offers a variable the stack never reads",
			mutate(func(t *PortainerTemplates) {
				t.Templates[0].Env = append(t.Templates[0].Env, PortainerEnv{Name: "UNUSED", Label: "unused", Default: "x"})
			}),
			RulePortainerTemplate, "the stack reads it nowhere"},
		{"defaults a variable to something the env file does not",
			mutate(func(t *PortainerTemplates) { t.Templates[0].Env[0].Default = "/somewhere/else" }),
			RulePortainerTemplate, "compose/backup-manager.env declares"},
		{"never offers a variable the stack needs",
			mutate(func(t *PortainerTemplates) { t.Templates[0].Env = nil }),
			RulePortainerTemplate, "never offers it"},
		{"renders a blank form field",
			mutate(func(t *PortainerTemplates) { t.Templates[0].Env[0].Label = "" }),
			RulePortainerTemplate, "no label"},
		{"has no logo, so the catalogue entry cannot render",
			mutate(func(t *PortainerTemplates) { t.Templates[0].Logo = "" }),
			RulePortainerTemplate, "declares no logo"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found := CheckPortainerTemplate("fixture", tc.tpl, vars, env, stackfile)
			if len(found) == 0 {
				t.Fatal("the template check passed")
			}
			matched := false
			for _, v := range found {
				if v.Rule == tc.rule && strings.Contains(v.Detail, tc.want) {
					matched = true
				}
			}
			if !matched {
				t.Errorf("the check failed, but for a different reason than %s/%q:\n%s", tc.rule, tc.want, format(found))
			}
		})
	}
}

// TestNoNewAdapterIsAPluginAnAgentOrAnAPIClient is #170's non-goal,
// stated in the issue so nobody helpfully adds one: no Portainer plugin,
// no Portainer API dependency, no Portainer agent, no Dockge plugin.
//
// It runs over all four rather than over Portainer alone. Two of the four
// names in that sentence are Dockge's, and an adapter that ships only
// prose today is exactly the one somebody would think has room for a
// small helper.
func TestNoNewAdapterIsAPluginAnAgentOrAnAPIClient(t *testing.T) {
	for _, a := range newAdapters() {
		t.Run(a.id, func(t *testing.T) {
			root := PlatformDir(a.id)
			n, err := countFiles(root)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				t.Fatal("this adapter's directory holds no files, so a clean scan proves nothing")
			}
			findings, err := ScanForProductPlugin(root)
			if err != nil {
				t.Fatalf("ScanForProductPlugin: %v", err)
			}
			if len(findings) > 0 {
				t.Errorf("this adapter has grown into a plugin, an agent or an API client:\n%s", format(findings))
			}
		})
	}
}

// TestTheProductPluginScanFires proves ScanForProductPlugin can see the
// thing it exists for, against a fixture rather than against the tree.
func TestTheProductPluginScanFires(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "clean.yml"), "services:\n  a:\n    image: x\n")
	if v, err := ScanForProductPlugin(dir); err != nil || len(v) > 0 {
		t.Fatalf("the clean fixture reported %v (err %v), so the control below proves nothing", v, err)
	}

	for _, tc := range []struct{ name, body, want string }{
		{"an agent", "services:\n  agent:\n    image: portainer/agent:latest\n", "portainer/agent"},
		{"an API client", "url: https://portainer.example/api/endpoints\n", "/api/endpoints"},
		{"an API key", "PORTAINER_API_KEY=redacted\n", "PORTAINER_API_KEY"},
		{"a Dockge plugin", "path: dockge/plugin/index.js\n", "dockge/plugin"},
		{"an API-key header", "headers:\n  X-API-Key: ${TOKEN}\n", "X-API-Key"},
		{"an API import", "import \"github.com/portainer/portainer-ce/api/client\"\n", "portainer-ce/api"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := t.TempDir()
			write(t, filepath.Join(d, "thing.yml"), tc.body)
			v, err := ScanForProductPlugin(d)
			if err != nil {
				t.Fatal(err)
			}
			if len(v) == 0 {
				t.Fatalf("%s went unnoticed", tc.name)
			}
			if !strings.Contains(oneLine(v), tc.want) {
				t.Errorf("the scan fired for a different reason than %q: %s", tc.want, oneLine(v))
			}
		})
	}

	// Markdown is skipped on purpose: a README saying "no Portainer agent
	// is required" is documentation, and flagging it is how a check gets
	// switched off. Both directions, so the skip cannot silently widen.
	d := t.TempDir()
	write(t, filepath.Join(d, "README.md"), "This adapter needs no portainer/agent and never mounts docker.sock.\n")
	if v, err := ScanForProductPlugin(d); err != nil || len(v) > 0 {
		t.Errorf("prose about not needing an agent was reported as needing one: %v (err %v)", v, err)
	}
}

// swapCase is the opposite spelling of whatever the rule stores: every
// letter of the marker in the other case. It is deliberately mechanical
// rather than a hand-picked variant, so the control below cannot be
// written to agree with the rule by accident.
func swapCase(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLower(r):
			return unicode.ToUpper(r)
		case unicode.IsUpper(r):
			return unicode.ToLower(r)
		}
		return r
	}, s)
}

// TestTheProductPluginScanIgnoresCase is the control the marker list had
// no way to fail before: every fixture in TestTheProductPluginScanFires
// spells its marker exactly as the rule stores it, so a scan that matched
// only that one spelling read clean and looked proven.
//
// It iterates productPluginMarkers itself rather than a copy of it, which
// is the part that keeps working: a marker added later arrives with its
// opposite-case control already written, and cannot be the one spelling
// nobody checked.
func TestTheProductPluginScanIgnoresCase(t *testing.T) {
	for _, m := range productPluginMarkers {
		t.Run(m.marker, func(t *testing.T) {
			other := swapCase(m.marker)
			if other == m.marker {
				t.Fatalf("%q has no other case, so this control asserts nothing", m.marker)
			}

			// The negative half, first: the same fixture with the marker
			// taken out has to read clean, or the hit below is the
			// surrounding YAML rather than the marker.
			d := t.TempDir()
			write(t, filepath.Join(d, "thing.yml"), "value: harmless\n")
			if v, err := ScanForProductPlugin(d); err != nil || len(v) > 0 {
				t.Fatalf("the fixture without the marker reported %v (err %v), so the hit below proves nothing", v, err)
			}

			d = t.TempDir()
			write(t, filepath.Join(d, "thing.yml"), "value: "+other+"\n")
			v, err := ScanForProductPlugin(d)
			if err != nil {
				t.Fatal(err)
			}
			if len(v) == 0 {
				t.Fatalf("%q went unnoticed, and it is the same dependency as %q", other, m.marker)
			}
			if !strings.Contains(oneLine(v), m.marker) {
				t.Errorf("the scan fired for a different reason than %q: %s", m.marker, oneLine(v))
			}
		})
	}
}

// TestTheProductPluginScanCatchesTheOrdinarySpellings pins the four
// spellings the #204 review named, which are all at least as likely as
// the ones the rule happens to store: the HTTP/2 header form, the shell
// env var form, an uppercased path segment, and a capitalised import
// path.
func TestTheProductPluginScanCatchesTheOrdinarySpellings(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"the header form", "headers:\n  x-api-key: ${TOKEN}\n", "X-API-Key"},
		{"the env var form", "portainer_api_key=redacted\n", "PORTAINER_API_KEY"},
		{"an uppercased path", "url: https://portainer.example/API/endpoints\n", "/api/endpoints"},
		{"a capitalised plugin path", "path: Dockge/Plugin/index.js\n", "dockge/plugin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := t.TempDir()
			write(t, filepath.Join(d, "thing.yml"), tc.body)
			v, err := ScanForProductPlugin(d)
			if err != nil {
				t.Fatal(err)
			}
			if len(v) == 0 {
				t.Fatalf("%s went unnoticed", tc.name)
			}
			if !strings.Contains(oneLine(v), tc.want) {
				t.Errorf("the scan fired for a different reason than %q: %s", tc.want, oneLine(v))
			}
		})
	}

	// Markdown stays skipped whatever the case: the reason it is skipped
	// is that a README explaining the absence of a plugin is prose, and
	// that does not change when the prose is capitalised.
	d := t.TempDir()
	write(t, filepath.Join(d, "README.md"), "This adapter needs no Portainer/Agent and sets no X-Api-Key.\n")
	if v, err := ScanForProductPlugin(d); err != nil || len(v) > 0 {
		t.Errorf("prose was reported as a dependency once it was capitalised: %v (err %v)", v, err)
	}
}

// ---------------------------------------------------------------------
// Dockge: the adapter that is deliberately not a package
// ---------------------------------------------------------------------

func TestDockgeShipsNoRuntimeDefinitionOfItsOwn(t *testing.T) {
	root := PlatformDir("dockge")

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	if len(entries) == 0 {
		t.Fatal("apps/dockge/ is empty; the import workflow is the deliverable and it has to be written down somewhere")
	}

	found, err := ScanForForkedStack(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Errorf("apps/dockge/ ships a runtime definition, and Dockge's whole support model is that it deploys the canonical stack itself:\n%s", format(found))
	}

	// The control, against a fixture: a directory that does hold a stack
	// has to be caught, or the clean result above is a scan that walked
	// nothing.
	fixture := t.TempDir()
	write(t, filepath.Join(fixture, "README.md"), "# prose\n")
	if v, err := ScanForForkedStack(fixture); err != nil || len(v) > 0 {
		t.Fatalf("a directory of prose was reported as a fork: %v (err %v)", v, err)
	}
	write(t, filepath.Join(fixture, "compose.yaml"), "services:\n  a:\n    image: x\n")
	v, err := ScanForForkedStack(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) == 0 {
		t.Error("a compose file appearing in the adapter went unnoticed, so the emptiness above is a convention rather than a check")
	}
}

// TestTheDockgeStackIsTheCanonicalStack pins the paths apps/dockge's
// import workflow tells an operator to use to container/.env.example.
//
// Without this, the Dockge column would be the only one whose host paths
// nothing compares against anything: it has no env file of its own for
// the storage-role rule to read, because the file it deploys is the
// canonical one.
func TestTheDockgeStackIsTheCanonicalStack(t *testing.T) {
	c := MustLoad()
	platform, ok := c.Platforms["dockge"]
	if !ok {
		t.Fatal("canonical.json declares no dockge platform")
	}

	env, err := ReadEnvFile(filepath.Join(RepoRoot, "container", ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range Roles {
		container, _ := c.ContainerPaths.ByRole(role)
		want, _ := platform.HostPaths.ByRole(role)
		got := ""
		for _, svc := range canonicalStackServices(t) {
			for _, m := range svc.Mounts {
				if m.ContainerPath == container {
					got = m.HostPath
				}
			}
		}
		if got == "" {
			t.Errorf("the canonical stack mounts nothing at %s, so the %q role has no host path to pin", container, role)
			continue
		}
		if got != want {
			t.Errorf("the canonical stack mounts the %q role from %s and canonical.json's dockge entry declares %s; the import workflow in apps/dockge/README.md would tell an operator to create the wrong directory", role, got, want)
		}
	}
	if len(env) == 0 {
		t.Fatal("container/.env.example set nothing, so every comparison above resolved against inline defaults and proves less than it looks")
	}

	// And the README actually names the files it claims to import. A
	// workflow page that has drifted off the artifact is the failure this
	// adapter is most exposed to, because the page IS the adapter.
	readme, err := os.ReadFile(filepath.Join(PlatformDir("dockge"), "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"container/compose.yaml", "container/.env.example"} {
		if !strings.Contains(string(readme), want) {
			t.Errorf("apps/dockge/README.md never mentions %s, and that file is what this adapter deploys", want)
		}
	}
}

// ---------------------------------------------------------------------
// CasaOS and ZimaOS store metadata
// ---------------------------------------------------------------------

func TestCasaOSAndZimaOSStoreMetadataDescribesTheStackBesideIt(t *testing.T) {
	c := MustLoad()

	for _, id := range []string{"casaos", "zimaos"} {
		t.Run(id, func(t *testing.T) {
			path := filepath.Join(PlatformDir(id), "compose", "backup-manager.yml")
			md, err := ReadCasaOSMetadata(path)
			if err != nil {
				t.Fatalf("read x-casaos: %v", err)
			}
			svcs, err := ReadCompose(path, nil)
			if err != nil {
				t.Fatalf("read compose: %v", err)
			}
			rt, drift := ReduceToRoles(id, svcs, c)
			if len(drift) > 0 {
				t.Fatalf("could not reduce this adapter to roles:\n%s", FormatDrift(drift))
			}
			if v := CheckCasaOSMetadata(id+"/compose/backup-manager.yml", md, rt, c); len(v) > 0 {
				t.Errorf("the store metadata does not describe the services beside it:\n%s", format(v))
			}
		})
	}
}

// TestTheStoreMetadataCheckFailsOnADeliberateMismatch is the control, and
// it mutates a deep copy of the real metadata rather than a hand-built
// fixture, so every branch runs against the shape actually shipped.
func TestTheStoreMetadataCheckFailsOnADeliberateMismatch(t *testing.T) {
	c := MustLoad()
	path := filepath.Join(PlatformDir("casaos"), "compose", "backup-manager.yml")
	base, err := ReadCasaOSMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	svcs, err := ReadCompose(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	rt, _ := ReduceToRoles("casaos", svcs, c)
	if v := CheckCasaOSMetadata("fixture", base, rt, c); len(v) > 0 {
		t.Fatalf("the real metadata already fails, so no control below means anything:\n%s", format(v))
	}

	copyMD := func() CasaOSMetadata {
		out := CasaOSMetadata{Store: base.Store, Services: map[string]CasaOSService{}}
		out.Store.Architectures = append([]string(nil), base.Store.Architectures...)
		out.Store.Title = copyStrings(base.Store.Title)
		out.Store.Tagline = copyStrings(base.Store.Tagline)
		out.Store.Description = copyStrings(base.Store.Description)
		out.Store.Tips.BeforeInstall = copyStrings(base.Store.Tips.BeforeInstall)
		for k, v := range base.Services {
			out.Services[k] = v
		}
		return out
	}

	cases := []struct {
		name string
		mut  func(*CasaOSMetadata)
		want string
	}{
		{"no icon", func(m *CasaOSMetadata) { m.Store.Icon = "" }, "declares no icon"},
		{"no title", func(m *CasaOSMetadata) { m.Store.Title = map[string]string{} }, "title.en_us"},
		{"no category", func(m *CasaOSMetadata) { m.Store.Category = "" }, "declares no category"},
		{"no description", func(m *CasaOSMetadata) { m.Store.Description = map[string]string{} }, "description.en_us"},
		{"no architectures", func(m *CasaOSMetadata) { m.Store.Architectures = nil }, "claims no architectures"},
		{"an architecture the release does not build", func(m *CasaOSMetadata) {
			m.Store.Architectures = []string{"amd64", "riscv64"}
		}, "the release builds"},
		{"a port_map the runtime does not listen on", func(m *CasaOSMetadata) { m.Store.PortMap = "9090" }, "port_map"},
		{"the engine behind the app tile", func(m *CasaOSMetadata) { m.Store.Main = rt.Engine.Name }, "that is the engine"},
		{"a service with no presentation at all", func(m *CasaOSMetadata) { delete(m.Services, rt.Engine.Name) }, "has no x-casaos block"},
		{"a mount the install dialog hides", func(m *CasaOSMetadata) {
			svc := m.Services[rt.Engine.Name]
			svc.Volumes = svc.Volumes[:1]
			m.Services[rt.Engine.Name] = svc
		}, "describes no such volume"},
		{"a described volume with no text", func(m *CasaOSMetadata) {
			svc := m.Services[rt.Engine.Name]
			svc.Volumes = append([]struct {
				Container   string            `yaml:"container"`
				Description map[string]string `yaml:"description"`
			}(nil), svc.Volumes...)
			svc.Volumes[0].Description = map[string]string{}
			m.Services[rt.Engine.Name] = svc
		}, "with no en_us text"},
		{"a published port nothing describes", func(m *CasaOSMetadata) {
			svc := m.Services[rt.WebUI.Name]
			svc.Ports = nil
			m.Services[rt.WebUI.Name] = svc
		}, "describes no such port"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := copyMD()
			tc.mut(&md)
			found := CheckCasaOSMetadata("fixture", md, rt, c)
			if len(found) == 0 {
				t.Fatal("the store-metadata check passed")
			}
			if !strings.Contains(oneLine(found), tc.want) {
				t.Errorf("it failed for a different reason than %q:\n%s", tc.want, format(found))
			}
			if v := CheckCasaOSMetadata("fixture", base, rt, c); len(v) > 0 {
				t.Errorf("the mutation reached the shared metadata:\n%s", format(v))
			}
		})
	}
}

func copyStrings(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// TestStoreMetadataStaysInTheDistributionAdapter is #170's acceptance
// criterion in its own words: "WHEN the Go core and the shared UI are
// scanned, THEN no CasaOS or ZimaOS import appears in either". Widened to
// the Portainer and Dockge concerns for the same reason, since all four
// are store or manager presentation.
func TestStoreMetadataStaysInTheDistributionAdapter(t *testing.T) {
	for _, tree := range []string{"core", filepath.Join("ui", "shared", "src")} {
		t.Run(tree, func(t *testing.T) {
			found, err := ScanForStoreMetadataLeak(Path(tree))
			if err != nil {
				t.Fatal(err)
			}
			if len(found) > 0 {
				t.Errorf("%s carries store or manager metadata:\n%s", tree, format(found))
			}
		})
	}

	// The control. A clean result over two large trees is exactly the
	// shape that hides a scanner walking nothing.
	fixture := t.TempDir()
	write(t, filepath.Join(fixture, "ok.go"), "package p\n\nconst Name = \"backup-manager\"\n")
	if v, err := ScanForStoreMetadataLeak(fixture); err != nil || len(v) > 0 {
		t.Fatalf("a clean fixture reported %v (err %v)", v, err)
	}
	write(t, filepath.Join(fixture, "leak.go"), "package p\n\n// TODO: read the x-casaos block here\n")
	v, err := ScanForStoreMetadataLeak(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) == 0 {
		t.Error("a CasaOS concern in a Go file went unnoticed, so the clean results above are a scan that found nothing rather than a tree that holds nothing")
	}
}

// ---------------------------------------------------------------------
// The frontend-bridge declaration, both directions
// ---------------------------------------------------------------------

// TestTheUIBridgeDeclarationIsCheckedInBothDirections is the control for
// the rule that replaced an unconditional storage-mount pin.
//
// The risk in that change is specific and worth naming: "skip the
// platforms that have no bridge" would have made `uiBridge: "none"` a way
// to switch the pin off. So the rule is two-sided, and both sides are
// exercised here against directories this test built.
func TestTheUIBridgeDeclarationIsCheckedInBothDirections(t *testing.T) {
	const mount = "/srv/example/backups"
	bridged := Platform{UIBridge: "apps/example/frontend/platform.ts", StorageMount: mount}
	none := Platform{UIBridge: UIBridgeNone, UIBridgeNote: "no payload to carry a bundle in", StorageMount: mount}

	withBridge := func(t *testing.T, declared string) string {
		t.Helper()
		dir := t.TempDir()
		write(t, filepath.Join(dir, "frontend", "platform.ts"),
			"export const b = {\n  deployment: {\n    storageMount: \""+declared+"\"\n  }\n};\n")
		return dir
	}

	t.Run("a matching bridge passes", func(t *testing.T) {
		if v := CheckUIBridgeDeclaration("example", bridged, withBridge(t, mount)); len(v) > 0 {
			t.Errorf("%s", format(v))
		}
	})
	t.Run("a drifted bridge fails", func(t *testing.T) {
		v := CheckUIBridgeDeclaration("example", bridged, withBridge(t, "/srv/somewhere/else"))
		if len(v) == 0 {
			t.Fatal("a bridge declaring a different storage mount passed")
		}
		if !strings.Contains(oneLine(v), "storageMount") {
			t.Errorf("it failed for a different reason: %s", oneLine(v))
		}
	})
	t.Run("a missing bridge fails when one is declared", func(t *testing.T) {
		if v := CheckUIBridgeDeclaration("example", bridged, t.TempDir()); len(v) == 0 {
			t.Error("a platform declaring a bridge it does not ship passed")
		}
	})
	t.Run("declaring none passes only when there is none", func(t *testing.T) {
		if v := CheckUIBridgeDeclaration("example", none, t.TempDir()); len(v) > 0 {
			t.Errorf("%s", format(v))
		}
		v := CheckUIBridgeDeclaration("example", none, withBridge(t, mount))
		if len(v) == 0 {
			t.Fatal("a platform with a frontend directory declared uiBridge \"none\" and passed, so \"none\" is a way to switch the storage-mount pin off")
		}
		if !strings.Contains(oneLine(v), "exists") {
			t.Errorf("it failed for a different reason: %s", oneLine(v))
		}
	})
	t.Run("declaring none needs a reason", func(t *testing.T) {
		silent := none
		silent.UIBridgeNote = ""
		if v := CheckUIBridgeDeclaration("example", silent, t.TempDir()); len(v) == 0 {
			t.Error("a platform shipping no bridge and saying nothing about why passed")
		}
	})
	t.Run("declaring nothing at all fails", func(t *testing.T) {
		if v := CheckUIBridgeDeclaration("example", Platform{StorageMount: mount}, t.TempDir()); len(v) == 0 {
			t.Error("a platform that has not said whether it ships a bridge passed")
		}
	})
}

// ---------------------------------------------------------------------
// Acceptance procedures
// ---------------------------------------------------------------------

// TestTheNewAdaptersAcceptanceProceduresAreSafeAndVerifiable covers the
// four procedures #170 adds. Three of them are also reached through
// allPlatforms(); Dockge's is not, because Dockge is in no platform
// fixture, and a procedure nothing checks is exactly the one that gets a
// `chown -R` over a backup root.
func TestTheNewAdaptersAcceptanceProceduresAreSafeAndVerifiable(t *testing.T) {
	c := MustLoad()
	for _, a := range newAdapters() {
		t.Run(a.id, func(t *testing.T) {
			backups := c.Platforms[a.id].HostPaths.Backups
			if backups == "" {
				t.Fatalf("canonical.json declares no backup root for %s, so the rules below would compare against an empty string and pass everything", a.id)
			}
			v, err := ReadAcceptanceProcedure(Path(a.acceptance), backups, map[string]string{})
			if err != nil {
				t.Fatalf("read %s: %v", a.acceptance, err)
			}
			if len(v) > 0 {
				t.Errorf("%s:\n%s", a.acceptance, format(v))
			}
		})
	}
}

// TestEveryNewProcedureRequiresAConfigBeforeInstall is the #204 review's
// M1. All four adapters gate the container that publishes a port on the
// engine reporting healthy, the engine's healthcheck is `status`, and
// `status` exits non-zero until a valid config.yaml exists on disk. A
// procedure whose step 0 creates the config directory and never puts a
// file in it therefore asks the operator to confirm "both containers
// reach running" and "the published port loads the shared web UI" after
// an install that cannot get there, and the destructive-safety re-check
// these four procedures exist to produce is unreachable behind it.
//
// The remedy is the procedure's, not the adapter's: container/compose.yaml
// carries the identical healthcheck and the identical service_healthy
// gate, and PropHealthCheck compares both, so an adapter that softened
// either would fail this suite's own equivalence gate. Fixing the health
// contract is #176's, and this is the interim handling #176 names.
func TestEveryNewProcedureRequiresAConfigBeforeInstall(t *testing.T) {
	for _, a := range newAdapters() {
		t.Run(a.id, func(t *testing.T) {
			v, err := ReadConfigPrecondition(Path(a.acceptance))
			if err != nil {
				t.Fatalf("read %s: %v", a.acceptance, err)
			}
			if len(v) > 0 {
				t.Errorf("%s:\n%s", a.acceptance, format(v))
			}
		})
	}
}

// TestTheConfigPreconditionIsTheEstablishedShape holds the rule above to
// the procedures #175 already wrote it into. If it passes on the four new
// ones and fails on these three, it is describing wording I invented
// rather than the handling this repository settled on.
func TestTheConfigPreconditionIsTheEstablishedShape(t *testing.T) {
	for _, name := range []string{
		"truenas-provider-acceptance.md",
		"unraid-provider-acceptance.md",
		"openmediavault-provider-acceptance.md",
	} {
		t.Run(name, func(t *testing.T) {
			v, err := ReadConfigPrecondition(Path(filepath.Join("docs", "acceptance", name)))
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			if len(v) > 0 {
				t.Errorf("docs/acceptance/%s:\n%s", name, format(v))
			}
		})
	}
}

// TestTheConfigPreconditionRuleFires is the control the clean results
// above need: one fixture per requirement, each missing exactly one
// thing, and each checked for the violation it is supposed to raise
// rather than for any violation at all.
func TestTheConfigPreconditionRuleFires(t *testing.T) {
	const (
		head      = "# A procedure\n\n## Step 0 — Prerequisites\n\n"
		file      = "Write `config.yaml` into the config directory before you install.\n\n"
		refusal   = "A missing or invalid config is a hard startup failure, not a first-run wizard.\n\n"
		citation  = "Serving a first-run flow instead is #176's work and is not merged.\n\n"
		checklist = "- [ ] `config.yaml` written inside it\n\n"
		install   = "## Step 1 — Install\n\n- [ ] Both containers reach `running`\n"
	)

	complete := head + file + refusal + citation + checklist + install
	if v := CheckConfigPrecondition(complete); len(v) > 0 {
		t.Fatalf("the complete fixture was reported as incomplete, so every control below proves nothing:\n%s", format(v))
	}

	for _, tc := range []struct{ name, doc, want string }{
		{
			"no config.yaml anywhere before the install",
			head + refusal + citation + "- [ ] the SSH key written and owned\n\n" + install,
			"never names `config.yaml`",
		},
		{
			"no statement that the engine refuses to start",
			head + file + citation + checklist + install,
			"hard startup failure",
		},
		{
			"no #176 citation",
			head + file + refusal + checklist + install,
			"#176",
		},
		{
			"no checklist box",
			head + file + refusal + citation + install,
			"checklist box",
		},
		{
			"the precondition sits after the install step",
			head + "- [ ] the host recorded\n\n" + install + "\n## Step 2 — Configure\n\n" + file + refusal + citation + checklist,
			"never names `config.yaml`",
		},
		{
			"no install step at all",
			head + file + refusal + citation + checklist,
			"`## Step 1` heading",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := CheckConfigPrecondition(tc.doc)
			if len(v) == 0 {
				t.Fatalf("a procedure with %s passed", tc.name)
			}
			// Every violation is read, not the one-line summary: that
			// summary elides after three entries, and a fixture missing
			// the whole precondition raises four.
			found := false
			for _, got := range v {
				if strings.Contains(got.Detail, tc.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("the rule fired for a different reason than %q:\n%s", tc.want, format(v))
			}
			for _, got := range v {
				if got.Rule != RuleMissingConfigPrecondition {
					t.Errorf("reported rule %q, and this fixture only breaks the config precondition", got.Rule)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

// distinctVarNames returns every ${VAR} name a compose file references,
// sorted and deduplicated.
func distinctVarNames(body string) []string {
	seen := map[string]bool{}
	var out []string
	for _, ref := range VarRefs(body) {
		if seen[ref.Name] {
			continue
		}
		seen[ref.Name] = true
		out = append(out, ref.Name)
	}
	sort.Strings(out)
	return out
}
