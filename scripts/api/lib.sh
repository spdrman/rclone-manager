# Shared definitions for the /api/v1 contract gates (issue #166).
#
# One place names the contract, its two generated outputs and how the
# generator is invoked, so the generate script and the drift check cannot
# disagree about any of the three. They disagreeing is the exact failure
# that would make a green drift check meaningless.

# shellcheck disable=SC2034
API_CONTRACT="api/v1/openapi.json"
# shellcheck disable=SC2034
API_GO_BINDING="apps/common/webhost/apicontract/contract.gen.go"
# shellcheck disable=SC2034
API_TS_BINDING="ui/shared/src/api/generated/contract.ts"

# api::generate <out-go> <out-ts>
#
# GOWORK=off, and a bare `go run <file>`: the generator is a single
# stdlib-only program that deliberately belongs to no module, exactly like
# scripts/architecture/ownership.go next to it, so it runs identically in a
# fresh worktree with nothing installed.
api::generate() {
  GOWORK=off go run scripts/api/gen-bindings.go . "$1" "$2"
}
