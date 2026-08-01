import { compat, types as T } from "../deps.ts";

// Nothing to migrate yet: this is the first release. Declared so that the next
// one has somewhere to go, and so that installing over a future version fails
// loudly rather than silently keeping a configuration it cannot read.
export const migration: T.ExpectedExports.migration = compat.migrations
  .fromMapping({}, "0.5.0");
