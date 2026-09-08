import { describe, expect, it } from "vitest";
import {
  addCommand,
  importCredentialsCommand,
  preflightCandidateCommand,
  preflightCommand,
  removeCommand
} from "@shared/pages/storageDestinationCommands";
import type { StorageMediumSpec } from "@shared/api/contracts";

/**
 * EPIC G's standing rule is that every action taken in the browser prints
 * the `backup-manager` command that would do the same thing. A wizard is
 * the hardest case for that rule and the case where it pays best: an
 * operator who configured one destination by clicking has, by the end,
 * read the exact command that configures the next fifty.
 *
 * The rule has one hard edge, and it is what most of this file is about.
 * The terminal these lines go to is copy-to-clipboard and exportable, so
 * anything printed there ends up pasted into a chat window eventually. A
 * printed command that carried an access key or a secret would be a real
 * leak, and redaction is deliberately not the answer: it is a policy
 * somebody has to remember at every print site. There is instead no flag
 * on the CLI that takes a secret at all, so there is nothing here to
 * redact, and these tests hold that structurally rather than by trusting
 * it.
 */

const CANARY_KEY_ID = "AKIAEXAMPLE594NOTREAL";
const CANARY_SECRET = "CANARY-594-ui-8b3f0c7d21ae-DO-NOT-PRINT";

const SPEC: StorageMediumSpec = {
  id: "offsite_s3",
  type: "s3",
  region: "us-east-1",
  bucket: "nas-backups",
  prefix: "monthly",
  storageClass: "STANDARD_IA",
  uploadVerification: "readback",
  credentials: { credentialsId: "9b41c7e2" }
};

describe("the echoed backup-manager command", () => {
  it("names the credential by reference and never carries material", () => {
    const printed = [
      importCredentialsCommand(),
      preflightCandidateCommand(SPEC),
      addCommand(SPEC),
      preflightCommand("offsite_s3"),
      removeCommand("offsite_s3")
    ].join("\n");

    // The positive control. Without it a renderer that returned empty
    // strings would pass every assertion below for the wrong reason.
    expect(printed).toContain("backup-manager medium add offsite_s3");

    for (const forbidden of [CANARY_SECRET, CANARY_KEY_ID, "--access-key-id", "--secret-access-key", "--secret"]) {
      expect(printed).not.toContain(forbidden);
    }
  });

  it("takes the material on stdin, so it is not in the process table", () => {
    expect(importCredentialsCommand()).toBe("backup-manager medium import-credentials --stdin");
  });

  it("renders the whole add, so what is printed is what actually works", () => {
    expect(addCommand(SPEC)).toBe(
      "backup-manager medium add offsite_s3 --type s3 --region us-east-1 " +
        "--bucket nas-backups --prefix monthly --storage-class STANDARD_IA " +
        "--upload-verification readback --credentials-id 9b41c7e2"
    );
  });

  it("omits the flags that were not filled in rather than printing empty ones", () => {
    expect(addCommand({ id: "minimal", type: "s3", bucket: "b", credentials: { credentialsId: "c" } })).toBe(
      "backup-manager medium add minimal --type s3 --bucket b --credentials-id c"
    );
  });

  it("spells the candidate preflight as the same flags the add takes", () => {
    expect(preflightCandidateCommand(SPEC)).toBe(
      "backup-manager medium preflight --candidate offsite_s3 --type s3 --region us-east-1 " +
        "--bucket nas-backups --prefix monthly --storage-class STANDARD_IA " +
        "--upload-verification readback --credentials-id 9b41c7e2"
    );
  });

  it("quotes a credential command so the printed line is copy-pasteable as typed", () => {
    const line = addCommand({
      id: "vaulted",
      type: "s3",
      bucket: "b",
      credentials: { command: ["op", "read", "op://vault/s3/creds"] }
    });
    expect(line).toContain("--credentials-command 'op read op://vault/s3/creds'");
  });

  it("names the reference sources without inventing a value for them", () => {
    expect(addCommand({ id: "a", type: "s3", bucket: "b", credentials: { file: "/etc/backup-manager/s3.creds" } }))
      .toContain("--credentials-file /etc/backup-manager/s3.creds");
    expect(addCommand({ id: "a", type: "s3", bucket: "b", credentials: { env: "BACKUP_S3_OFFSITE" } }))
      .toContain("--credentials-env BACKUP_S3_OFFSITE");
  });

  it("prints the two settings-list buttons as the commands they are", () => {
    expect(preflightCommand("offsite_s3")).toBe("backup-manager medium preflight offsite_s3");
    expect(removeCommand("offsite_s3")).toBe("backup-manager medium remove offsite_s3");
  });
});
