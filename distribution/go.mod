// distribution/ is the distribution layer (issue #165, EPIC B #81's
// standing constraint "one runtime, thin adapters"): packaging metadata,
// templates, store presentation, and the conformance suite that holds every
// provider package to one canonical description.
//
// Its own module, deliberately. Before Phase 6 this code lived in
// apps/common/packaging, inside the module that also holds the /api/v1 host
// and the shared authentication service, which put the distribution layer
// and the core application layer behind one go.mod and made "core does not
// depend on distribution" a claim nobody could check. Splitting it means the
// dependency direction is enforced by the module graph itself and not only
// by a scanner: scripts/architecture/check-core-dependency-rule.sh reads the
// layer of every module from scripts/architecture/layers.conf and refuses a
// core-layer module that imports this one.
//
// One dependency, gopkg.in/yaml.v3, carried over unchanged from
// apps/common: the conformance suite parses real Compose and catalog YAML
// rather than pattern-matching it as text, because a check that reads a
// mount declaration as a string is a check a reformatting can defeat. It
// pins the same version apps/common already pinned, so the move introduces
// no new dependency and no new version to reconcile. See
// distribution/README.md.
module github.com/spdrman/rclone-manager/distribution

go 1.27.0

require gopkg.in/yaml.v3 v3.0.1
