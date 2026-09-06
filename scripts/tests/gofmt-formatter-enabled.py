#!/usr/bin/env python3
"""Does this .golangci.yml enable the gofmt formatter? (issue #417)

Exit 0 when formatters.enable lists gofmt, 1 when it does not, 2 when the
question cannot be answered (no yaml module, unreadable file).

Read structurally rather than by grepping for the word, because
.golangci.yml carries several paragraphs explaining why the formatter is
enabled, and a scan that matched those would pass against a config that had
lost the setting and kept the prose. Group L of
scripts/tests/ci-local-gate.test.sh drives this and mutates a copy of the
config to prove it can say no.
"""
import sys

try:
    import yaml
except ImportError:
    sys.exit(2)

try:
    with open(sys.argv[1]) as fh:
        doc = yaml.safe_load(fh) or {}
except OSError as err:
    print(err, file=sys.stderr)
    sys.exit(2)

enabled = ((doc.get("formatters") or {}).get("enable")) or []
sys.exit(0 if "gofmt" in enabled else 1)
