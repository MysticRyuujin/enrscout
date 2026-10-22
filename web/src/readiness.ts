import { useEffect, useState } from "react";
import { fetchForks } from "./api";
import type { ForkReadiness, Readiness } from "./types";

// For a cancelled fork, ready means the node no longer schedules it, so every label inverts.
export type ForkMode = "scheduled" | "activated" | "cancelled";

export function forkMode(phase: ForkReadiness["phase"]): ForkMode {
  return phase === "activated" || phase === "cancelled" ? phase : "scheduled";
}

export function stateLabel(state: Readiness, mode: ForkMode): string {
  switch (state) {
    case "ready":
      return mode === "activated"
        ? "upgraded"
        : mode === "cancelled"
          ? "fork unscheduled"
          : "fork scheduled";
    case "pending":
      return "pending";
    case "not_ready":
      return mode === "activated"
        ? "left behind"
        : mode === "cancelled"
          ? "still scheduled"
          : "not scheduled";
    case "mismatch":
      return "other schedule";
    case "unknown":
      return "no schedule advertised";
    case "stale":
      return "older fork";
  }
}

// Only the states a layer's bars can draw: stale rows never reach a client row, execution rows are
// never unknown, after activation mismatch is folded into left behind, and a cancelled fork has no
// pending or mismatch.
export function legendStates(layer: "el" | "cl", mode: ForkMode): Readiness[] {
  if (mode === "cancelled")
    return layer === "el"
      ? ["ready", "not_ready"]
      : ["ready", "not_ready", "unknown"];
  if (mode === "activated")
    return layer === "el"
      ? ["ready", "pending", "not_ready"]
      : ["ready", "pending", "not_ready", "unknown"];
  return layer === "el"
    ? ["ready", "not_ready", "mismatch"]
    : ["ready", "not_ready", "mismatch", "unknown"];
}

export function stateHint(state: Readiness, mode: ForkMode): string {
  if (mode === "cancelled") {
    switch (state) {
      case "ready":
        return "The identity no longer schedules the cancelled fork: its own schedule names no next fork, or a different one.";
      case "not_ready":
        return "The identity still schedules the cancelled fork, or has already moved onto it. It forks away from the network at the time the fork would have activated.";
      case "unknown":
        return "The record carries no eth2 entry, or its digest does not match the fork observed over Status, so its schedule cannot be read.";
      default:
        return "The identity is not on the current fork.";
    }
  }
  switch (state) {
    case "ready":
      return mode === "activated"
        ? "The identity advertises the new fork."
        : "The identity names this fork in its own schedule: the execution fork ID Next field, or the consensus record's next fork version and epoch.";
    case "pending":
      return "The identity scheduled the fork, but its last observation is still on the fork before it. The crawler has not seen it since activation, or the node has not synced to the fork.";
    case "not_ready":
      return mode === "activated"
        ? "The identity still advertises the fork before this one, without this one in its schedule."
        : "The identity advertises no next fork: Next is 0, or the next fork epoch is far-future.";
    case "mismatch":
      return "The identity advertises a next fork that is not this one: a custom or overridden fork time, or a client bug in how it encodes the schedule.";
    case "unknown":
      return "The record carries no eth2 entry, or its digest does not match the fork observed over Status, so its schedule cannot be read.";
    case "stale":
      return "The identity is not on the current fork.";
  }
}

// One layer can be cancelled while the other is still scheduled for the same instant.
export function layerForkMode(d: ForkReadiness, layer: "el" | "cl"): ForkMode {
  return forkMode(d.fork[layer]?.phase ?? d.phase);
}

// The fork view a layer is in, for pages that show a per-node readiness without the Forks data.
export function useForkMode(network: string, layer: "el" | "cl"): ForkMode {
  const [mode, setMode] = useState<ForkMode>("scheduled");
  useEffect(() => {
    let live = true;
    setMode("scheduled");
    fetchForks(network)
      .then((d) => live && setMode(layerForkMode(d, layer)))
      .catch(() => {});
    return () => {
      live = false;
    };
  }, [network, layer]);
  return mode;
}
