import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { EnrollmentPage } from "@shared/auth/EnrollmentPage";
import { LoginPage } from "@shared/auth/LoginPage";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { BackupManagerError } from "@shared/api/contracts";
import type { ApiErrorCode } from "@shared/api/contracts";
import type { BackupManagerApi } from "@shared/api/contracts";

/**
 * Issue #274. An operator opened the enrolment link Backup Manager itself
 * printed, after the token had lapsed, and was told:
 *
 *     The administrator account could not be created.
 *     Advanced details -> correlation id cid_enroll
 *
 * while the response on the wire said BOOTSTRAP_TOKEN_INVALID, "missing,
 * expired or already-used bootstrap token", correlationId cid_0KII_WE8.
 * Wrong cause, no recovery, and an identifier that appears in no log.
 *
 * These tests are about what the operator is told, so they assert on
 * rendered text rather than on the mapping function underneath: the
 * mapping being right and the page still showing its old fixed sentence
 * is exactly the failure #274 is.
 */

/** Not a real token: the header value is never asserted on here, only
 *  whether the link carried one at all. */
const A_LINK_WITH_A_TOKEN = "/enroll?token=placeholder-value-for-this-test";

function apiRefusing(code: ApiErrorCode, message: string, correlationId: string): BackupManagerApi {
  const api = createMockApi();
  const rejection = new BackupManagerError({ code, message, correlationId });
  vi.spyOn(api, "enrollAdministrator").mockRejectedValue(rejection);
  vi.spyOn(api, "login").mockRejectedValue(rejection);
  return api;
}

function renderEnrollment(api: BackupManagerApi) {
  return render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <EnrollmentPage onEnrolled={() => {}} />
      </ApiProvider>
    </MemoryRouter>
  );
}

async function enroll() {
  const username = screen.getByLabelText("Username");
  const password = screen.getByLabelText(/^Password/);
  const confirm = screen.getByLabelText("Confirm password");
  await userEvent.type(username, "bm-admin");
  await userEvent.type(password, "a-long-enough-passphrase");
  await userEvent.type(confirm, "a-long-enough-passphrase");
  await userEvent.click(screen.getByRole("button", { name: "Create administrator" }));
}

async function signIn(api: BackupManagerApi) {
  render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <LoginPage onSignedIn={() => {}} />
      </ApiProvider>
    </MemoryRouter>
  );
  await userEvent.type(screen.getByLabelText("Username"), "bm-admin");
  await userEvent.type(screen.getByLabelText("Password"), "a-long-enough-passphrase");
  await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
}

beforeEach(() => {
  window.history.replaceState({}, "", "/enroll");
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  window.history.replaceState({}, "", "/");
});

describe("enrolment says why it refused", () => {
  it("blames the lapsed link, not the credentials just typed", async () => {
    window.history.replaceState({}, "", A_LINK_WITH_A_TOKEN);
    renderEnrollment(
      apiRefusing("BOOTSTRAP_TOKEN_INVALID", "missing, expired or already-used bootstrap token", "cid_0KII_WE8")
    );
    await enroll();

    await screen.findByText(/expired or has already been used/i);
    // The sentence that sent the operator back to retype a password that
    // was never the problem.
    expect(screen.queryByText(/administrator account could not be created/i)).toBeNull();
    expect(screen.getByText(/were not the problem/i)).toBeInTheDocument();
  });

  it("says how to get a working link, since a web page is the one place that cannot show one", async () => {
    window.history.replaceState({}, "", A_LINK_WITH_A_TOKEN);
    renderEnrollment(
      apiRefusing("BOOTSTRAP_TOKEN_INVALID", "missing, expired or already-used bootstrap token", "cid_0KII_WE8")
    );
    await enroll();

    const remediation = await screen.findByText(/Restart Backup Manager/);
    expect(remediation.textContent).toMatch(/log/i);
  });

  it("shows the correlation id the response carried, and never a literal", async () => {
    window.history.replaceState({}, "", A_LINK_WITH_A_TOKEN);
    renderEnrollment(
      apiRefusing("BOOTSTRAP_TOKEN_INVALID", "missing, expired or already-used bootstrap token", "cid_0KII_WE8")
    );
    await enroll();

    await screen.findByText("correlation id cid_0KII_WE8");
    expect(document.body.textContent).not.toContain("cid_enroll");
  });

  it("tells a link with no token apart from a link whose token has lapsed", async () => {
    // Same refusal on the wire. The browser can settle this one itself.
    renderEnrollment(
      apiRefusing("BOOTSTRAP_TOKEN_INVALID", "missing, expired or already-used bootstrap token", "cid_no_token")
    );
    await enroll();

    const message = await screen.findByText(/opened without an enrolment token/i);
    expect(message).toBeInTheDocument();
    expect(screen.getByText(/\?token=/)).toBeInTheDocument();
    expect(screen.queryByText(/expired or has already been used/i)).toBeNull();
  });

  it("sends an operator who has already enrolled to sign in, not to mint another token", async () => {
    window.history.replaceState({}, "", A_LINK_WITH_A_TOKEN);
    renderEnrollment(apiRefusing("ENROLLMENT_CLOSED", "an administrator account already exists", "cid_closed"));
    await enroll();

    await screen.findByText(/administrator account already exists on this instance/i);
    expect(screen.getByRole("link", { name: /sign in/i })).toBeInTheDocument();
    expect(screen.queryByText(/Restart Backup Manager/)).toBeNull();
  });

  it("says to wait when the address is rate limited", async () => {
    window.history.replaceState({}, "", A_LINK_WITH_A_TOKEN);
    renderEnrollment(
      apiRefusing("RATE_LIMITED", "too many enrollment attempts; wait before trying again", "cid_rate")
    );
    await enroll();

    await screen.findByText(/too many attempts from this address/i);
    expect(screen.getByText(/Wait a minute/)).toBeInTheDocument();
  });

  it("offers no correlation id at all when the request never reached the service", async () => {
    window.history.replaceState({}, "", A_LINK_WITH_A_TOKEN);
    const api = createMockApi();
    vi.spyOn(api, "enrollAdministrator").mockRejectedValue(new TypeError("Failed to fetch"));
    renderEnrollment(api);
    await enroll();

    await screen.findByText(/did not answer/i);
    // Nothing was logged under any id, so there is no id to quote and no
    // advanced details to open.
    expect(document.body.textContent).not.toContain("correlation id");
    expect(screen.queryByText("Advanced details")).toBeNull();
  });
});

describe("signing in says why it refused", () => {
  it("shows the correlation id the response carried, and never a literal", async () => {
    await signIn(apiRefusing("UNAUTHENTICATED", "that username and password combination was not accepted", "cid_9Xq"));

    await screen.findByText(/username and password combination was not accepted/i);
    await waitFor(() => expect(screen.getByText("correlation id cid_9Xq")).toBeInTheDocument());
    expect(document.body.textContent).not.toContain("cid_login");
  });

  it("does not report a rate-limited address as a wrong password", async () => {
    await signIn(apiRefusing("RATE_LIMITED", "too many login attempts; wait before trying again", "cid_rate"));

    await screen.findByText(/too many attempts from this address/i);
    expect(screen.queryByText(/combination was not accepted/i)).toBeNull();
  });
});
