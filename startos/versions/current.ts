import { IMPOSSIBLE, VersionInfo } from '@start9labs/start-sdk'
import { appVersion, packageRevision } from '../version'

export const current = VersionInfo.of({
  version: `${appVersion}:${packageRevision}`,
  releaseNotes: {
    en_US:
      'Fixes for what Forktower says to a user who already has a watchtower. ' +
      'On a fresh install it could report that your channels were protected ' +
      'by somebody else\'s watchtower and that this works — said once, before ' +
      'the watchtower Forktower runs here had even finished starting. A tower ' +
      'somebody else runs watches whichever chain their node follows, which ' +
      'cannot be seen from here and changes when they upgrade; the tower here ' +
      'is the one with a known view of the chain your own node cannot see. ' +
      'Forktower now asks you to register it, alongside whatever you already ' +
      'use rather than instead of it, and the setup list no longer counts a ' +
      'third-party tower as having covered that step.',
  },
  migrations: {
    up: async ({ effects }) => {},
    down: IMPOSSIBLE,
  },
})
