# Store listing copy

One description, used by every target that has a listing. A per-store rewrite is a
per-store opportunity for one of them to say something the product does not do, and a
reviewer comparing two listings from the same project is exactly the reader who notices.

Keep this file and the `description` field in each provider's own metadata in step:
`apps/truenas/catalog/app.yaml`, `apps/unraid/template/*.xml`'s `<Overview>`, and the
Synology package's `INFO` description. Those files hold the short forms; this one holds
the text they are shortened from.

## Name

Backup Manager

## One line

Pulls backup artifacts off a remote SFTP source on a schedule, verifies them, and retains
them under a policy you set.

## Short description (under 200 characters)

Scheduled, verified, pull-based backups from an SFTP source to your NAS, with a durable
catalog of every artifact it holds and a retention policy you confirm before it deletes
anything.

## Full description

Backup Manager runs on your NAS and pulls backup artifacts off a remote SFTP source on a
schedule you choose. Every artifact it fetches is verified against the hash the source
published before it counts as retained, and every artifact it holds is recorded in a
durable local catalog, so the question "do I still have Tuesday's database dump, and is it
intact" has an answer that does not involve reading a directory listing and hoping.

Retention is a two-step operation on purpose. The app computes a plan, shows you exactly
which restore points it proposes to delete, and applies only the plan you confirmed. A
plan that has gone stale because something changed underneath it deletes nothing and asks
again. Nothing is removed on a schedule you cannot see beforehand.

It is a backup *manager*, not a backup agent: it does not create the backups, it collects
the ones your database, hypervisor or application already writes, and it takes
responsibility for getting them off that machine, checking them, keeping the right ones and
telling you when that stops happening.

The web interface covers normal configuration and monitoring without a terminal: adding a
source, pinning its SSH host key, defining backup sets, watching runs, previewing and
applying retention, and reading the current health of every set. Alerting is proactive
rather than a dashboard you have to remember to open: a stale set, a run that keeps
failing, a changed host key and critical storage pressure are all surfaced without being
asked.

## What it does not do

Stated in the listing because a reviewer reads it and an administrator installing on the
strength of the first paragraph should not discover it afterwards.

- It does not create backups. It collects and retains backups something else produced.
- It does not restore into production for you. Recovery is a documented procedure that
  tells you which local artifact is trustworthy and where it is.
- It does not talk to any cloud service of ours. There is none.
- It does not browse arbitrary remote files, and it is not a general-purpose SFTP client.

## Categories and keywords

Category: Backup. Keywords: backup, sftp, rclone, retention, catalog, verification.
