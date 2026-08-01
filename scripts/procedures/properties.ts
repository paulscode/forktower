import { compat, types as T } from "../deps.ts";

// What the Properties page shows.
//
// Written by the container at start into `start9/stats.yaml`, because the only
// thing worth putting here is what Forktower has actually found — which chain
// each node is on, and whether they have separated. A page of constants would
// be a page nobody reads twice.
export const properties: T.ExpectedExports.properties = compat.properties;
