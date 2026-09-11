"""The repository's own scripts, as one code base rather than sixty files.

Every domain directory under `scripts/` grew the same four things
independently: a way to print a step, a way to refuse and say why, a way
to wait for something with a deadline, and a private idea of what its
exit status means to whoever is reading it. Sixty files, roughly thirty
thousand lines, and the vocabulary was reinvented in each one, which is
why two scripts that both mean "this machine could not perform the
proof" said it with two different numbers.

`bdtools.harness` is the one place that vocabulary lives now. Domain
packages beside it (`bdtools.e2e`, and the others as they port) hold the
entry points, and nothing in them redefines `die`, `note`, `step`,
`wait_or_die`, the exit-code taxonomy, or the help renderer.

Standard library only, Python 3.8 or newer, for the reason
`scripts/install/install_docker_host.py` gives at length: these scripts
run on appliances and CI runners where installing a dependency is not
something the operator is allowed to do. Nothing here imports outside the
standard library, and nothing here is allowed to start doing so.
"""
