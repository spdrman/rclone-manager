/**
 * Issue #598. What a failure this frontend could not type is allowed to
 * turn into on screen.
 *
 * The defect these drive out is not that a request failed, it is that both
 * ends threw away every fact about how. `useAsync` caught the exception,
 * bound it to a name it never read, and substituted one fixed sentence
 * plus the literal correlation id `unavailable`, which is worse than no id
 * at all: `ErrorState` only offers its Advanced details disclosure when a
 * failure carried an id, so a literal one buys an operator a panel to open
 * with nothing behind it.
 *
 * So each case here asks the same question in a different shape: is what
 * reaches the operator identifiable? A thrown `SyntaxError` has to read as
 * a body that could not be parsed, a `fetch` that never came back has to
 * read as no answer at all, and neither is allowed to invent an id.
 */
import { describe, expect, it } from "vitest";
import { BackupdError, RequestFailure } from "./contracts";
import { asApiError, describeFailure } from "./failure";

describe("describeFailure keeps what the exception said", () => {
  it("names a body it could not read, rather than substituting a fixed sentence", () => {
    const failure = describeFailure(
      new RequestFailure({
        kind: "unreadable-body",
        path: "/activity",
        status: 200,
        contentType: "text/html; charset=utf-8",
        cause: new SyntaxError("Unexpected token '<', \"<!doctype \"... is not valid JSON")
      }),
      "Activity could not be loaded."
    );

    expect(failure.message).toMatch(/could not read/i);
    // The three facts that separate this from every other failure: what
    // was asked for, what came back, and what the parser said about it.
    expect(failure.detail).toContain("/api/v1/activity");
    expect(failure.detail).toContain("200");
    expect(failure.detail).toContain("text/html");
    expect(failure.detail).toContain("SyntaxError");
    expect(failure.detail).toContain("is not valid JSON");
  });

  it("offers no correlation id for a request that never reached the service", () => {
    const failure = describeFailure(
      new RequestFailure({ kind: "no-response", path: "/activity", cause: new TypeError("Failed to fetch") }),
      "Activity could not be loaded."
    );

    expect(failure.message).toMatch(/did not answer/i);
    expect(failure.correlationId).toBeUndefined();
    expect(failure.detail).toContain("TypeError");
    expect(failure.detail).toContain("Failed to fetch");
  });

  it("quotes the id a 2xx response carried when its body could not be read", () => {
    const failure = describeFailure(
      new RequestFailure({
        kind: "unreadable-body",
        path: "/activity",
        status: 200,
        contentType: "application/json",
        correlationId: "cid_abc123",
        cause: new SyntaxError("Unexpected end of JSON input")
      }),
      "Activity could not be loaded."
    );

    expect(failure.correlationId).toBe("cid_abc123");
  });

  it("keeps an exception it cannot classify instead of dropping it", () => {
    // A mapper throwing inside `.then()` is the shape #598 was reported
    // as: `request()` resolved, and `r.events.map` did not survive what it
    // resolved with. Nothing types that, so the exception's own words are
    // the only thing there is to show.
    const failure = describeFailure(
      new TypeError("r.events.map is not a function"),
      "Activity could not be loaded."
    );

    expect(failure.correlationId).toBeUndefined();
    expect(failure.detail).toContain("TypeError");
    expect(failure.detail).toContain("r.events.map is not a function");
  });

  it("never mints the literal correlation id 'unavailable'", () => {
    for (const e of [
      new TypeError("Failed to fetch"),
      new SyntaxError("Unexpected end of JSON input"),
      new RequestFailure({ kind: "no-response", path: "/activity", cause: new Error("boom") }),
      new Error("something nobody anticipated")
    ]) {
      const failure = describeFailure(e, "Activity could not be loaded.");
      expect(failure.correlationId).not.toBe("unavailable");
    }
  });
});

describe("asApiError is the one conversion the fetch hooks use", () => {
  it("hands a typed refusal through with its own code and id", () => {
    const api = asApiError(
      new BackupdError({ code: "NOT_CONFIGURED", message: "this instance has no configuration", correlationId: "cid_x1" })
    );

    // isNotConfigured() reads .code off this, so the code has to survive
    // the trip or every "nothing is set up yet" empty state turns into a
    // red banner.
    expect(api.code).toBe("NOT_CONFIGURED");
    expect(api.message).toBe("this instance has no configuration");
    expect(api.correlationId).toBe("cid_x1");
  });

  it("keeps the service's own sentence for an INTERNAL refusal and adds where to look", () => {
    const api = asApiError(
      new BackupdError({ code: "INTERNAL", message: "failed to list activity", correlationId: "cid_x2" })
    );

    expect(api.message).toBe("failed to list activity");
    expect(api.remediation).toMatch(/log/i);
    expect(api.correlationId).toBe("cid_x2");
  });

  it("carries an untyped exception's own words through as detail, with no id", () => {
    const api = asApiError(new SyntaxError("Unexpected token '<'"));

    expect(api.code).toBe("unknown");
    expect(api.correlationId).toBeUndefined();
    expect(api.detail).toContain("SyntaxError");
    expect(api.detail).toContain("Unexpected token '<'");
  });
});

/**
 * Issue #795's review. Which MACHINE a refusal came out of is a separate
 * question from what it says, and this module used to answer it by
 * looking at two things that cannot: the error code (where `unknown`
 * conflates "no typed envelope" with "a valid code this bundle has not
 * heard of") and the status (where 502/503/504 conflates "the proxy in
 * front of the service" with "the service, answering for itself").
 *
 * It reads recorded provenance now, and these are the four answers.
 */
describe("provenance decides which hop a failure names", () => {
  it("never speaks over a service that typed its own refusal, whatever the status", () => {
    const failure = describeFailure(
      new BackupdError({
        code: "unknown",
        message: "Backupd is in a maintenance window until 04:00.",
        status: 503,
        origin: "service"
      }),
      "Activity could not be loaded."
    );

    expect(failure.message).toBe("Backupd is in a maintenance window until 04:00.");
    expect(failure.remediation).toBeUndefined();
    expect(failure.origin).toBe("service");
  });

  it("names the hop when the proxy in front of the service said it wrote the refusal", () => {
    const failure = describeFailure(
      new BackupdError({
        code: "unknown",
        message: "The backup service returned an unexpected response.",
        status: 502,
        correlationId: "cid_marked",
        origin: "gateway"
      }),
      "Activity could not be loaded."
    );

    expect(failure.message).toMatch(/could not reach the Backupd service/i);
    expect(failure.correlationId).toBe("cid_marked");
    expect(failure.origin).toBe("gateway");
  });

  it("falls back to the gateway wording for an untyped gateway status, which older builds are", () => {
    // serve-ui's marker is newer than this classification, and another
    // proxy between the browser and the service would not set it at all.
    // An untyped 502 is still a response with nothing of the service's in
    // it, so the wording holds.
    const failure = describeFailure(
      new BackupdError({
        code: "unknown",
        message: "The backup service returned an unexpected response.",
        status: 502,
        origin: "unknown"
      }),
      "Activity could not be loaded."
    );

    expect(failure.message).toMatch(/could not reach the Backupd service/i);
  });

  it("names no hop at all for a refusal nothing established the origin of", () => {
    // A hand-built ApiError, a mock, or a page's own state. Absent
    // provenance is "not established", and guessing one from the status
    // is the defect, not the fix.
    const failure = describeFailure(
      new BackupdError({ code: "unknown", message: "something nobody typed", status: 502 }),
      "Activity could not be loaded."
    );

    expect(failure.message).toBe("something nobody typed");
    expect(failure.message).not.toMatch(/could not reach the Backupd service/i);
  });
});
