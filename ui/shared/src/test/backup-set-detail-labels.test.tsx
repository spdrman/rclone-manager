/**
 * What the backup set detail page says about the connection, and one thing
 * it must stop saying.
 *
 * The Connection section used to state "Algorithm: ssh-ed25519" beside an
 * empty fingerprint, and the algorithm was a literal in the JSX: no field
 * on `BackupSet` or `BackupSetHealth` carries a host key or its algorithm,
 * so nothing chose it. On a deployment whose trust anchor is an RSA key the
 * page named a key type the operator never trusted, next to no fingerprint
 * to check it against, and the halt banner for a CHANGED host key sent them
 * here to do exactly that comparison.
 *
 * That is the removal dialog's argument one surface over: showing nothing
 * beats a confident wrong answer, and it beats it hardest where somebody
 * might compare what the page says against what their server offers.
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

function renderDetail() {
  return render(
    <MemoryRouter initialEntries={[backupSetPath("production", "postgres-primary")]}>
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
  it("does not name a host key algorithm the page never read", async () => {
    renderDetail();
    const heading = await screen.findByRole("heading", { name: "Connection" });
    const section = heading.closest("section") ?? heading.parentElement!;

    // "ssh-ed25519" was a literal in the JSX. Nothing on the wire carries
    // a host key algorithm for a backup set, so on a deployment pinning
    // an RSA key this named the wrong key type beside an empty
    // fingerprint.
    expect(within(section).queryByText(/ssh-ed25519/)).toBeNull();
    expect(within(section).queryByText(/^Algorithm$/)).toBeNull();
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
    // And it says where the trusted host key is decided, rather than
    // leaving a section that quietly lost a row.
    expect(within(section).getByText(/trusted host key/i)).toBeInTheDocument();
  });
});
