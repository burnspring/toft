import { describe, expect, it } from "vitest";

import {
  deployStanding,
  firstDeployFailed,
  lastDeployFailedOverServing,
  servingLatest,
} from "./deploy-state";

const failed = { id: "b2", status: "failed" };
const ok = { id: "b1", status: "success" };

describe("deployStanding", () => {
  it("is never_served when nothing is serving", () => {
    // The row the old derivations folded into their reassuring branch.
    expect(deployStanding(undefined, undefined)).toBe("never_served");
    expect(deployStanding(failed, undefined)).toBe("never_served");
    expect(deployStanding(ok, undefined)).toBe("never_served");
  });

  it("is serving_latest when the serving row is the newest", () => {
    expect(deployStanding(ok, ok)).toBe("serving_latest");
  });

  it("is serving_latest when there are no build rows but something serves", () => {
    // History can be pruned out from under a running release.
    expect(deployStanding(undefined, ok)).toBe("serving_latest");
  });

  it("is serving_older when the newest build is not what runs", () => {
    expect(deployStanding(failed, ok)).toBe("serving_older");
  });
});

describe("the failure banner picks one of two sentences", () => {
  it("says the app kept serving only when it did", () => {
    expect(lastDeployFailedOverServing(failed, ok)).toBe(true);
    expect(firstDeployFailed(failed, ok)).toBe(false);
  });

  it("says the first deploy failed when nothing has ever served", () => {
    // The defect: this row used to take the "still serving the previous
    // release … Nothing is down" branch, three inches above a Health card
    // reading "Nothing deployed yet".
    expect(lastDeployFailedOverServing(failed, undefined)).toBe(false);
    expect(firstDeployFailed(failed, undefined)).toBe(true);
  });

  it("says neither when the newest build worked", () => {
    expect(lastDeployFailedOverServing(ok, ok)).toBe(false);
    expect(firstDeployFailed(ok, undefined)).toBe(false);
  });

  it("says neither when there are no builds at all", () => {
    expect(lastDeployFailedOverServing(undefined, undefined)).toBe(false);
    expect(firstDeployFailed(undefined, undefined)).toBe(false);
  });
});

describe("servingLatest", () => {
  it("is false for an app that has never served", () => {
    // The other half of the same defect: a green tick and "Serving the latest
    // build" for an app serving nothing.
    expect(servingLatest(failed, undefined)).toBe(false);
    expect(servingLatest(ok, undefined)).toBe(false);
  });

  it("is false when there is no build to be up to date with", () => {
    expect(servingLatest(undefined, undefined)).toBe(false);
    expect(servingLatest(undefined, ok)).toBe(false);
  });

  it("is true only when the serving row is the newest build", () => {
    expect(servingLatest(ok, ok)).toBe(true);
    expect(servingLatest(failed, ok)).toBe(false);
  });
});
