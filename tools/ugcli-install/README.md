# UGCLI for UGREEN / UGOS Pro

Two ways to get UGREEN `ugcli` onto an x86_64 / amd64 UGREEN NAS running UGOS
Pro. They differ in one respect that matters, so pick deliberately.

| | `ugcli-installer.sh` | `dpkg/*.deb` |
|---|---|---|
| Version | **tracks the newest UGREEN publishes** | pinned at 1.1.0.13 |
| Needs the internet at install time | yes, to discover and download | yes, to download |
| Artifact reviewed in this repo | no, fetched at run time | yes, with a committed SHA-256 |
| Rollback story | pin with `UGCLI_VERSION=` | reinstall the same `.deb` |

Use the **script** for a development NAS you want kept current. Use the **`.deb`**
when you need the exact bytes that were reviewed, or a repeatable install.

## How the script finds "the newest"

UGREEN publishes no `latest` pointer, no manifest and no directory listing.
Checked on 2026-08-29:

```text
/pro/ugcli/download/ugcli-latest-linux-amd64        404
/pro/ugcli/version, /version.json, /manifest.json   404
/pro/ugcli/ and /pro/ugcli/download/                200, empty bodies
```

What the endpoint does give is an enumerable, verifiable URL scheme, so the
script walks the version upward from the last one it was verified against until
the endpoint stops answering:

```text
ugcli-v1.1.0.12-linux-amd64   200
ugcli-v1.1.0.13-linux-amd64   200
ugcli-v1.1.0.14-linux-amd64   404
```

It keeps probing a little past the first miss rather than stopping dead, so a
withdrawn build cannot hide every release after it. It also separates "not
published" from "could not tell": a real 404 moves the walk on, while a network
or WAF failure stops it, because treating an unreachable server as "no newer
version" would silently pin you to the floor and call it the latest.

If discovery cannot run at all, the install proceeds at the floor version
rather than failing. A pinned install beats no install.

To pin deliberately:

```bash
UGCLI_VERSION=1.1.0.13 ./ugcli-installer.sh
```

## The `.deb` stays pinned, on purpose

`dpkg/ugcli-ugreen-bootstrap_1.1.0.13-1_amd64.deb` and its `.sha256` are
committed together so the artifact you install is the one that was reviewed.
Floating its version would defeat that, so its `postinst` keeps the hardcoded
`1.1.0.13`. Everything below this line describes **the `.deb` path**, and its
version numbers are correct for it.

Package:

```text
ugcli-ugreen-bootstrap_1.1.0.13-1_amd64.deb
```

## Requirements

- UGREEN NAS running UGOS Pro
- x86_64 / amd64 architecture
- SSH access
- A user with `sudo` access
- Python 3 with HTTPS/TLS support
- Internet access during installation

Check the NAS architecture:

```bash
uname -m
```

Expected:

```text
x86_64
```

Check Python:

```bash
python3 --version
```

## 1. Copy the `.deb` to the UGREEN NAS

For example, from another computer:

```bash
scp ugcli-ugreen-bootstrap_1.1.0.13-1_amd64.deb USER@UGREEN_IP:~/
```

Then SSH into the NAS:

```bash
ssh USER@UGREEN_IP
```

## 2. Optional: Verify the SHA-256 checksum

Expected SHA-256:

```text
130ebaba203f5bf2cab5df160d2ba0f8151488196caed3939477e78b7373bbba
```

Verify:

```bash
sha256sum ugcli-ugreen-bootstrap_1.1.0.13-1_amd64.deb
```

The reported checksum should match the value above exactly.

## 3. Install the package

Install with `dpkg`:

```bash
sudo dpkg -i ./ugcli-ugreen-bootstrap_1.1.0.13-1_amd64.deb
```

Do **not** use `apt install` for this package on a UGOS system with unresolved Debian package dependencies.

The package does not require APT to install `ugcli`.

## 4. Verify the installation

Run:

```bash
ugcli --version
```

Expected version:

```text
1.1.0.13
```

You can also confirm the executable:

```bash
command -v ugcli
```

Expected:

```text
/usr/local/bin/ugcli
```

Then test the CLI:

```bash
ugcli --help
```

## What the package does

The package is a bootstrap installer for the official UGREEN Linux x86_64 `ugcli` binary.

During installation it:

1. Checks that the NAS is x86_64 / amd64.
2. Checks that Python 3 is available.
3. Checks that Python HTTPS/TLS support is available.
4. Creates a unique temporary working directory.
5. Downloads the official UGREEN `ugcli` 1.1.0.13 binary.
6. Verifies that the downloaded file is a 64-bit x86-64 ELF executable.
7. Verifies that the downloaded CLI reports version `1.1.0.13`.
8. Installs the versioned executable under:

   ```text
   /opt/ugcli-ugreen/1.1.0.13/ugcli
   ```

9. Makes it available as:

   ```text
   /usr/local/bin/ugcli
   ```

10. Verifies the installed executable.

## APT safety

The package deliberately does **not** run any of the following:

```bash
apt-get update
apt-get install
apt --fix-broken install
apt upgrade
apt dist-upgrade
```

This is intentional.

Some UGOS installations contain vendor-managed packages that may not resolve cleanly against the configured Debian repositories. Automatically attempting to repair those dependencies can replace or remove UGOS-managed packages.

If a required prerequisite is missing, the installer stops and reports a **MANUAL ACTION REQUIRED** message instead of modifying the UGOS package dependency graph.

## Existing `ugcli` installation

If `/usr/local/bin/ugcli` already exists before this package is installed, the installer preserves the existing executable before replacing it.

This allows the previous installation to be restored if this package is later removed.

## Uninstall

Remove the package with:

```bash
sudo dpkg -r ugcli-ugreen-bootstrap
```

If the package preserved a previous `/usr/local/bin/ugcli`, it will attempt to restore that executable during removal.

To check whether the package is installed:

```bash
dpkg -l | grep ugcli-ugreen-bootstrap
```

## Reinstall

To reinstall the same package:

```bash
sudo dpkg -i ./ugcli-ugreen-bootstrap_1.1.0.13-1_amd64.deb
```

## Troubleshooting

### `sudo: command not found` or permission denied

Use a UGOS administrator account with sudo access.

Test:

```bash
sudo -v
```

### Unsupported architecture

Check:

```bash
uname -m
```

This package supports:

```text
x86_64
amd64
```

It does not install the x86_64 `ugcli` binary on ARM64 systems.

### Python is missing

Check:

```bash
python3 --version
```

If Python is not installed, install or restore Python 3 manually before retrying the `.deb`.

Avoid running `apt --fix-broken install` automatically on UGOS without first reviewing what packages APT proposes to change or remove.

### Python HTTPS/TLS support is missing

Check:

```bash
python3 -c 'import ssl, urllib.request; print(ssl.OPENSSL_VERSION)'
```

The command should complete successfully and print the OpenSSL version.

### Internet access failure

The bootstrap `.deb` downloads the official `ugcli` binary during installation, so the NAS must be able to reach UGREEN's download server over HTTPS.

Test general HTTPS connectivity with Python:

```bash
python3 - <<'PY'
import urllib.request
print(urllib.request.urlopen("https://developer.ugnas.com", timeout=15).status)
PY
```

### Check package status

```bash
dpkg -s ugcli-ugreen-bootstrap
```

### Check installed files

```bash
ls -l /usr/local/bin/ugcli
ls -l /opt/ugcli-ugreen/1.1.0.13/ugcli
```

## Basic usage

Show available commands:

```bash
ugcli --help
```

Create a project:

```bash
mkdir -p ~/ugreen-dev
cd ~/ugreen-dev

ugcli create my-app
```

Check a project:

```bash
ugcli check
```

Package a project:

```bash
ugcli pack --arch amd64 --build 1
```

## Package information

| Item | Value |
|---|---|
| Package | `ugcli-ugreen-bootstrap` |
| Package version | `1.1.0.13-1` |
| UGCLI version | `1.1.0.13` |
| Architecture | `amd64` / `x86_64` |
| CLI path | `/usr/local/bin/ugcli` |
| Versioned binary | `/opt/ugcli-ugreen/1.1.0.13/ugcli` |
| Installer type | Bootstrap Debian package |
