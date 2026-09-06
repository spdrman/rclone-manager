// apps/synology is the Synology DSM provider app (issue #85/B4.4,
// docs/EPIC-B-multi-nas.md §72 Work Package 4.4, D-5).
//
// It deliberately has NO dependency on core/ or apps/common. Unlike
// apps/generic, this app ships no Go binary of its own: the .spk carries
// the exact provider-neutral release binaries §3.7 requires ("the exact
// same core binary digest"), so there is nothing here to link against
// them. Everything in this module is packaging and conformance-checking
// machinery, which is why its dependency set is the standard library and
// nothing else.
module github.com/spdrman/rclone-manager/apps/synology

go 1.27.0
