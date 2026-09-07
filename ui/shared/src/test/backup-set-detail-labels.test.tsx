/**
 * What the backup set detail page says about the connection.
 *
 * The Connection section used to state "Algorithm: ssh-ed25519" beside an
 * empty fingerprint, and the algorithm was a literal in the JSX: nothing on
 * the wire carried a host key or its algorithm, so nothing chose either. On
 * a deployment whose trust anchor is an RSA key the page named a key type
 * the operator never trusted, next to no fingerprint to check it against,
 * and the halt banner for a CHANGED host key sent them here to do exactly
 * that comparison.
 *
 * That comparison is the point, so the answer was to carry the real key
 * rather than to keep the panel empty. The service now reports every key a
 * set pins, and these tests hold both halves of that: the algorithm shown
 * is the one the service read (checked against a set that pins ECDSA, so a
 * returning literal cannot pass), and the fingerprint is on the page for
 * the operator the banner sent here.
 *
 * The other thing this file was going to cover, and does not, is the
 * accessible name every row on this page lacks. It is real and it is
 * recorded on the pull request rather than fixed here: naming the values
 * collides with the inline edit form, whose fields carry four of the same
 * labels.
 */
import { afterEach, describe, expect, it } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { BackupSetDetailPage } from "@shared/pages/BackupSetDetailPage";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { resetGraphForTests } from "@shared/state/graph";
import { backupSetPath } from "@shared/utilities/routes";

function renderDetail(source = "production", set = "postgres-primary") {
  return render(
    <MemoryRouter initialEntries={[backupSetPath(source, set)]}>
      <ApiProvider api={createMockApi()}>
        <Routes>
          <Route path="/sets/:source/:set" element={<BackupSetDetailPage readOnly={false} />} />
        </Routes>
      </ApiProvider>
    </MemoryRouter>
  );
}

afterEach(() => {
  resetGraphForTests();
});

describe("the Connection section", () => {
  it("names the algorithm the service reported, not one written into the page", async () => {
    // production/auth-config pins an ECDSA key. The algorithm used to be
    // the literal "ssh-ed25519" in the JSX, so this set is the one that
    // catches a page naming a key type nobody chose: if the literal ever
    // comes back, the page says ed25519 about an ECDSA anchor and this
    // fails. The fingerprint is asserted alongside it because an algorithm
    // with nothing to check it against is what the old panel already had.
    renderDetail("production", "auth-config");
    const heading = await screen.findByRole("heading", { name: "Connection" });
    const section = heading.closest("section") ?? heading.parentElement!;

    expect(within(section).getByText(/ecdsa-sha2-nistp256/)).toBeInTheDocument();
    expect(within(section).queryByText(/ssh-ed25519/)).toBeNull();
    expect(
      within(section).getByText(/SHA256:1aXpQ8Lm\+Nb3vRt7yKcE0dJf5UwZoGqS2iTrHuVeM4k/)
    ).toBeInTheDocument();
  });

  it("still says the one thing about the connection that is true and useful", async () => {
    renderDetail();
    const heading = await screen.findByRole("heading", { name: "Connection" });
    const section = heading.closest("section") ?? heading.parentElement!;

    // The host is real, and the promise about the private key is a
    // property of the product rather than of this set, so both stay.
    expect(within(section).getByText("Host")).toBeInTheDocument();
    expect(within(section).getByText(/prod-db-01\.internal/)).toBeInTheDocument();
    expect(within(section).getByText(/private key never leaves this NAS/i)).toBeInTheDocument();
    // And the fingerprint the halt banner sends an operator here to
    // compare is actually on the page now.
    expect(
      within(section).getByText(/SHA256:9kQ2mVv\+Rt4hLc0pXeN1sJfB7yUwZaGdQ8oT3iKrEuM/)
    ).toBeInTheDocument();
  });
});
