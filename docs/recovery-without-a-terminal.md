# Recovery without a terminal

This is the recovery page for an administrator who has the web interface and nothing else:
no shell on the NAS, no SSH into it, no ability to open a database file by hand. That is
the normal case on a NAS appliance, and it is the case every one of the provider stores
this application is submitted to assumes.

`docs/recovery.md` is the other half. It covers the same ground for somebody who does have
a shell, and it goes further than this page can, because reading the catalog directly
answers questions the interface does not ask. Neither page replaces the other. If you have
a terminal, read that one; if you do not, everything below is reachable from the interface
and none of it needs one.

Three failures account for almost every message this project receives. Each gets a section
below: what you see, what it actually means, and what to do about it in the interface.

---

## 1. A backup has gone stale

### What you see

The dashboard's health summary shows one or more sets marked stale, with a warning badge
and a count. Opening the set shows its last successful run further in the past than the
schedule should allow.

### What it means

Stale is a statement about time, not about damage. It means no new artifact has arrived
for this set within the window its schedule implies. Everything already retained is still
there and still verified. What has stopped is the arrival of new material, and the
distinction matters because the fix is completely different depending on which side
stopped: this application, or the machine producing the backups.

### What to do

1. Open the set and read its recent runs. There are three shapes and they point in
   different directions.
   - **Runs are happening and finding nothing.** The source is not producing new backups,
     or is writing them somewhere other than the path this set watches. Nothing is wrong
     on the NAS. Check the machine that produces the backups, then confirm the remote path
     on this set is still the path it writes to.
   - **Runs are failing.** Read the error on the most recent one; it names which stage
     failed. If it is a connection error, section 3 is the likely cause. If it is a
     verification error, the artifact arrived but did not match the hash the source
     published, which means the transfer or the source is damaged, not this application.
   - **No runs at all since the last success.** The schedule is not firing. Confirm the
     set is enabled, and confirm the application itself is running: the dashboard is
     served by a different container from the engine, so the interface can be perfectly
     healthy while the engine is down. The health summary says so when that is the case.
2. Whatever the cause, nothing needs recovering yet. A stale set is a warning that your
   protection is ageing, not that it is gone.
3. Once new artifacts start arriving again the set clears itself. It does not need to be
   reset, and there is nothing to acknowledge.

### If you need a file back right now

Retained artifacts are ordinary files in the backup root you chose at install time, which
is a directory in your own share. You reach them the same way you reach anything else in
that share: your NAS's own file manager, or the network share on your desktop. The set's
detail page lists each artifact's name and the run that produced it, so you can identify
the one you want before you go looking for it.

Take the newest artifact whose state is retained and verified. Do not take one that is
still in flight or quarantined: those are exactly the two states that mean the application
does not vouch for the bytes.

---

## 2. A retention apply refused, or deleted nothing

### What you see

You previewed a retention plan, confirmed it, and the apply came back refusing, usually
saying the plan is stale. Nothing was deleted.

### What it means

This is the application working, not failing. Retention is deliberately two steps: it
computes a plan, shows you exactly which restore points it proposes to remove, and then
applies only that plan. If anything changed between the preview and the apply, a new
artifact arrived, a run completed, the policy was edited, the plan you confirmed is no
longer a description of the current state. Applying it anyway would delete a set of
restore points nobody looked at.

So it deletes nothing and asks again. A retention system that quietly re-derived the plan
at apply time would be one where the list you approved and the list it acted on are not the
same list, and there would be no way to tell from the outside.

### What to do

1. Preview again. You will get a fresh plan reflecting whatever changed.
2. Read it. It is a different plan from the one you read a moment ago, which is the whole
   reason the first one was refused, so it deserves the same look.
3. Confirm and apply. If it refuses a second time, something is arriving frequently enough
   to invalidate a plan between preview and apply. Pause the set's schedule, apply
   retention, then resume it.

### If the apply deleted less than the preview showed

It will not, and if you believe it has, check the preview you are comparing against: a
preview from before the last run describes a state the apply no longer saw. The set detail
page records what each apply actually removed, which is the authoritative answer.

### Nothing here can delete a backup you did not confirm

Worth stating plainly, because it is the fear behind most of these messages. Retention
never deletes on its own schedule, never deletes outside the plan you confirmed, and never
deletes anything at all when the plan has gone stale.

---

## 3. The SSH host key changed

### What you see

Runs for a set start failing with a host key error, and the health summary raises it as its
own alert rather than as a generic failure. Nothing is transferred.

### What it means

This is the most serious of the three and the one most likely to be mundane. When the set
was created you were shown the remote server's host key fingerprint and asked to confirm
it. That fingerprint was pinned. The server is now presenting a different one.

There are two explanations and they are not close together:

- The server was legitimately rebuilt, reinstalled, migrated, or had its host keys
  regenerated. Common, and usually something you or a colleague did on purpose.
- Something is between you and that server presenting itself as the server. Uncommon, and
  the entire reason the key was pinned in the first place.

The application cannot tell these apart, and it deliberately does not guess. It stops.

### What to do

1. **Do not clear the pin from the interface as a way of making the error go away.** That
   is the one action here that can turn a detected interception into a silent one.
2. Verify the new fingerprint out of band: on the source machine's own console, from the
   colleague who rebuilt it, from your provisioning system's record. Out of band means by
   a route that does not go through the connection you are trying to validate.
3. When you have the real fingerprint in front of you, open the set, choose to re-pin the
   host key, and compare the fingerprint the interface shows against the one you obtained.
   They must match character for character.
4. If they match, accept the new key. Runs resume on the next schedule.
5. If they do not match, stop and treat it as an incident. Nothing is lost: every artifact
   already retained is on your own storage, and the application refused to talk to the
   server rather than transferring anything to or from it.

### While it is unresolved

The set is stale and getting staler, and everything already retained is untouched and
still verified. You are losing new protection, not existing protection, which is why it is
safe to spend a day getting the fingerprint verified properly rather than clearing the pin
in the first ten minutes.

---

## When none of these is it

- **The interface loads but every page is empty or errors.** The web interface and the
  engine are separate containers. The interface is up and the engine is not. Restart the
  application through your NAS's own application manager; that is the supported control
  and it needs no terminal.
- **The interface does not load at all.** The application is stopped. Start it the same
  way.
- **You cannot sign in.** The administrator record lives in the application's state
  directory, which survives restarts and upgrades. Your platform's procedure for
  reinstalling the application while keeping its state is in that target's own
  documentation; reinstalling does not touch your retained artifacts, which live outside
  the application's state on purpose.
- **Anything else.** Open an issue with the set's detail page and the failing run's error
  message. https://github.com/spdrman/rclone-manager/issues
