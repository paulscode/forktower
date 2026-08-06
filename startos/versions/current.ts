import { IMPOSSIBLE, VersionInfo } from '@start9labs/start-sdk'
import { appVersion, packageRevision } from '../version'

export const current = VersionInfo.of({
  version: `${appVersion}:${packageRevision}`,
  releaseNotes: {
    en_US:
      'Forktower no longer watches the other chain until that chain\'s node ' +
      'has caught up. On a fresh install the second Bitcoin node is still ' +
      'doing its initial sync, and Forktower was taking its position in that ' +
      'sync as a starting point and following the download forward — scanning ' +
      'blocks from years before any of your channels existed, competing for ' +
      'disk with the sync itself, and raising an urgent "scanning has stopped" ' +
      'alert every time a read hiccupped under the load. Those alerts were ' +
      'also keyed so that one problem became a new urgent alert each time the ' +
      'height moved. Nothing was at risk: during that sync the second node ' +
      'cannot see the other chain anyway. Nothing on the dashboard should have ' +
      'suggested otherwise.',
  },
  migrations: {
    up: async ({ effects }) => {},
    down: IMPOSSIBLE,
  },
})
