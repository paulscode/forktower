import { compat, types as T } from "../deps.ts";
import { appVersion } from "../../startos/version.ts";
import { configSpec } from "./getConfig.ts";

/**
 * Fill in settings a saved config predates.
 *
 * **This exists because a missing field fails an install, not a validation.**
 * When StartOS reconfigures a package it checks the *saved* config against the
 * *current* spec, and a key that is absent is offered to the spec as null.
 * `ValueSpecBoolean` refuses null — there is no nullable boolean on this
 * platform — so a config written before a field was added does not merely lose
 * that field, it stops the update with `Field Is Not Nullable` and the package
 * does not install at all.
 *
 * Seen on hardware: a config saved when the second-node section had no
 * `onion-only` blocked the next install outright, months after the field was
 * added and with nothing in between to notice.
 *
 * Walking the spec rather than naming the fields is the point. A repair that
 * lists what was missing this time fixes this time; the next field added
 * quietly reintroduces the same failure for anybody who has not opened the
 * settings screen since.
 */
function fillMissing(spec: Record<string, unknown>, config: T.Config): T.Config {
  for (const [key, raw] of Object.entries(spec)) {
    const value = raw as Record<string, unknown>;
    switch (value.type) {
      case "object": {
        const nested = config[key];
        config[key] = fillMissing(
          value.spec as Record<string, unknown>,
          (typeof nested === "object" && nested !== null && !Array.isArray(nested)
            ? nested
            : {}) as T.Config,
        );
        break;
      }
      case "pointer":
        // Left alone on purpose: the platform dereferences these itself, and a
        // pointer accepts null in the meantime. Writing one here would put a
        // stale address in the file that the platform then overwrites anyway.
        break;
      default:
        if (config[key] === undefined) {
          // `default` is absent only on nullable fields, where null is the
          // honest value rather than a stand-in for one.
          config[key] = (value.default ?? null) as T.Config[string];
        }
    }
  }
  return config;
}

/**
 * **Keyed at the version that ships the repair, and `noRepeat` so it runs once.**
 *
 * `fromMapping` selects migrations above the version being upgraded from and at
 * or below the current one, so the current version has to be the real one — it
 * was left at `0.5.0` while the package shipped 0.6.x, which would have filtered
 * out every migration written after it. `check-versions.sh` keeps the manifest
 * and `startos/version.ts` in step; this reads the same constant so there is one
 * version in the tree rather than three.
 */
export const migration: T.ExpectedExports.migration = compat.migrations
  .fromMapping(
    {
      "0.6.10": {
        up: compat.migrations.updateConfig(
          (config) => fillMissing(configSpec as Record<string, unknown>, config),
          true,
          { version: "0.6.10", type: "up" },
        ),
        // Nothing to undo: filling in a field with the default it would have had
        // leaves a config an older version reads exactly as it did before.
        down: compat.migrations.updateConfig(
          (config) => config,
          true,
          { version: "0.6.10", type: "down" },
        ),
      },
    },
    appVersion,
  );
