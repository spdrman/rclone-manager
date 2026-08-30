#!/bin/sh
# Registered validator "trailer-marker" (docs/EPIC-B-multi-nas.md §71 Work
# Package 3.2, §26 Step 5): confirms the artifact's own content ends with a
# fixed completion trailer. A producer that wants this extra confirmation
# appends the trailer after it finishes writing the artifact; nothing on
# the remote or in this manager ever writes it, so its presence is
# evidence the *producer*, not this manager's own completion heuristics,
# considers the write finished.
#
# $1 is the local artifact path (see internal/lifecycle/verify.go's
# runValidator, the one caller of any FR-13 application validator: it
# invokes this script as `trailer-marker.sh <local-path>`, nothing more).
# Exit 0 (pass) only when the trailer is present in the file's final
# bytes; nonzero (fail) otherwise, including when the file cannot be read.
set -eu

tail -c 200 -- "$1" 2>/dev/null | grep -qF -- '--RCLONE-MANAGER-BACKUP-COMPLETE--'
