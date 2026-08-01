import { types as T } from "../deps.ts";

// Neither dependency needs configuring on our behalf.
//
// Bitcoin is read over its RPC with the credentials the platform already gives
// a dependent, and Lightning is read from the two files in its `public`
// directory. Forktower changes nothing about either — it has no code that
// could, which is the whole point of it.
export const dependencies: T.ExpectedExports.dependencies = {};
