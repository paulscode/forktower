import { IMPOSSIBLE, VersionInfo } from '@start9labs/start-sdk'
import { appVersion, packageRevision } from '../version'

export const current = VersionInfo.of({
  version: `${appVersion}:${packageRevision}`,
  releaseNotes: {
    en_US:
      'Two fixes for Forktower blaming the wrong thing while it starts up. If ' +
      'your channels were reported as not protected because your node had not ' +
      'negotiated a session with the watchtower, that was wrong: your node was ' +
      'asking, correctly and repeatedly, and Forktower's own watchtower ' +
      'cannot accept a session until the second Bitcoin node has finished ' +
      'syncing. That is now what it says, and it no longer raises an alert ' +
      'about it — the faster first sync is the way to shorten it. Separately, ' +
      'a Lightning node that had not been read yet was reported in red as one ' +
      'that could not be read, from the moment the app started until the first ' +
      'poll returned.',
  },
  migrations: {
    up: async ({ effects }) => {},
    down: IMPOSSIBLE,
  },
})
