import { IMPOSSIBLE, VersionInfo } from '@start9labs/start-sdk'
import { appVersion, packageRevision } from '../version'

export const current = VersionInfo.of({
  version: `${appVersion}:${packageRevision}`,
  releaseNotes: {
    en_US:
      'Two fixes for what happens when Forktower restarts. An urgent alert ' +
      'left over from a previous run could not be closed while Forktower was ' +
      'waiting for its second Bitcoin node to catch up — and waiting is ' +
      'exactly what it does for the first days of an install, so the alert ' +
      'would have stood that whole time. It now closes as soon as the node ' +
      'answers, whether or not there is anything to scan yet. And the second ' +
      'node is not listening at all for the first moment after a restart, ' +
      'which Forktower reported as the node being on the wrong network before ' +
      'exiting and being restarted. It now waits for the node to become ' +
      'answerable, as it already did for a node still loading its block index.',
  },
  migrations: {
    up: async ({ effects }) => {},
    down: IMPOSSIBLE,
  },
})
