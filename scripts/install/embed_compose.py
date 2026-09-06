#!/usr/bin/env python3
"""Regenerate the canonical runtime definition embedded in
install_docker_host.py (issue #346).

    python3 scripts/install/embed_compose.py

`install_docker_host.py` carries a copy of `container/compose.yaml` so an
operator can put one file on a NAS and run it. That copy is only safe
while it is provably the same definition, so
`TestEmbeddedComposeMatchesCanonical` holds the two together byte for
byte and this script is the only supported way to move it.

This is a script rather than a branch inside that test on purpose. The
regeneration used to live in the test itself, behind
`EMBED_COMPOSE_UPDATE=1`, which meant a member of the suite rewrote the
module the rest of the suite had already imported. A test run that edits
its own source is a test run whose result describes a file that no longer
exists.

The two refusals below are the other reason it moved. The embedded blob
is a NON-raw triple-quoted literal, so:

  * a backslash anywhere in the canonical file would be read back as an
    escape rather than as itself. The gate compares bytes, so it would
    fail; regenerating would splice the same backslash in again and
    produce the same mismatch; and the failure message would tell the
    operator to run the thing that had just failed. It could not
    converge, only loop.
  * a `\"\"\"` would close the literal early, leaving the rest of the
    compose file as Python source. That does not fail one test, it breaks
    the module at import and takes the whole installer suite with it.

Neither can happen today (`container/compose.yaml` has no backslash and
no triple quote), and both are one ordinary edit away. So they are
refusals with a real remedy rather than a comment nobody reads.
"""

from __future__ import annotations

import hashlib
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
INSTALLER = HERE / "install_docker_host.py"
CANONICAL = HERE.parents[1] / "container" / "compose.yaml"

# The literal is opened with `"""\` so the first line of the compose file
# is not preceded by a newline the canonical file does not have.
BLOB_OPEN = 'EMBEDDED_COMPOSE_YAML = """\\\n'
BLOB_CLOSE = '"""'
DIGEST_ASSIGN = "EMBEDDED_COMPOSE_SHA256 = "


class Unembeddable(Exception):
    """The canonical file cannot be carried in a Python string literal as
    it stands, and splicing it in anyway would fail in a way that
    regenerating cannot fix."""


def refuse_if_unembeddable(compose_text: str) -> None:
    """Refuse the two contents that cannot survive the splice.

    Both refusals are about a failure that regenerating cannot clear,
    which is why they are refusals and not warnings. The embedded copy
    is a non-raw triple-quoted literal: a backslash in the canonical
    file comes back as an escape rather than as itself, and a triple
    quote closes the literal early and leaves the rest of a compose file
    standing where Python source should be, which breaks the installer
    at import and takes its whole suite with it.

    The messages name both ways out (change the canonical file, or
    change how the definition is carried) because the person who hits
    this is holding a compose file they had a reason to write.
    """
    if "\\" in compose_text:
        raise Unembeddable(
            "container/compose.yaml now contains a backslash, and the embedded copy is a "
            "non-raw triple-quoted literal, so it would be read back as an escape rather than "
            "as itself.\n\n"
            "Regenerating cannot fix this: the gate compares bytes, the splice reintroduces the "
            "same backslash, and the message points back at this script. Either take the "
            "backslash out of the canonical file, or change the embedded literal to a raw "
            "string (r\"\"\"...\"\"\") and this splice with it."
        )
    if '"""' in compose_text:
        raise Unembeddable(
            'container/compose.yaml now contains a triple quote, which would close the embedded '
            'literal early and leave the rest of the compose file standing as Python source.\n\n'
            "That does not fail one test, it breaks install_docker_host.py at import and takes "
            "the whole installer suite with it. Take the triple quote out of the canonical file, "
            "or carry the definition as base64 rather than as a literal."
        )


def rewrite(module_src: str, compose_text: str) -> str:
    """`module_src` with the embedded blob and its digest replaced.

    Refuses first, so a splice that could not converge never happens.
    """
    refuse_if_unembeddable(compose_text)

    start = module_src.index(BLOB_OPEN) + len(BLOB_OPEN)
    # Provably the terminator rather than "the first one that turns up":
    # refuse_if_unembeddable has already established there is no triple
    # quote inside the content, so the next one is the literal's own.
    end = module_src.index(BLOB_CLOSE, start)
    out = module_src[:start] + compose_text + module_src[end:]

    digest = hashlib.sha256(compose_text.encode("utf-8")).hexdigest()
    d_start = out.index(DIGEST_ASSIGN) + len(DIGEST_ASSIGN)
    d_end = out.index("\n", d_start)
    return out[:d_start] + f'"{digest}"' + out[d_end:]


def main(argv=None) -> int:
    """Regenerate the embedded copy, or say why it did not.

    Already-identical is a success and prints so, because this is run to
    make a state true rather than to make a change, and a run that
    reports nothing done is the expected result on a clean tree.

    Reading and writing bytes with an explicit UTF-8 decode is not
    tidiness: read_text uses the locale's encoding, the canonical file
    carries a section sign and em dashes, and under LC_ALL=C it does not
    decode at all.
    """
    argv = list(sys.argv[1:] if argv is None else argv)
    canonical = Path(argv[0]) if argv else CANONICAL
    if not canonical.is_file():
        print(f"refusing: {canonical} is not there. This script regenerates the embedded copy "
              f"FROM the canonical file, so it needs a checkout.", file=sys.stderr)
        return 1

    # read_bytes, decoded explicitly: read_text() would use the locale's
    # encoding, and this file has a section sign and em dashes in it, so
    # under LC_ALL=C it does not decode at all.
    compose_text = canonical.read_bytes().decode("utf-8")
    before = INSTALLER.read_bytes().decode("utf-8")
    try:
        after = rewrite(before, compose_text)
    except Unembeddable as exc:
        print(f"refusing: {exc}", file=sys.stderr)
        return 1

    if after == before:
        print(f"{INSTALLER.name} already carries {canonical}, byte for byte. Nothing to do.")
        return 0
    INSTALLER.write_bytes(after.encode("utf-8"))
    print(f"regenerated the embedded definition in {INSTALLER.name} from {canonical}.\n"
          f"Re-run the installer suite to confirm, and commit {INSTALLER.name}.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
