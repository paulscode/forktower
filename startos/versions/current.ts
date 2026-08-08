import { IMPOSSIBLE, VersionInfo } from '@start9labs/start-sdk'
import { appVersion, packageRevision } from '../version'

export const current = VersionInfo.of({
  version: `${appVersion}:${packageRevision}`,
  releaseNotes: {
    en_US:
      'A second place where Forktower could lose track of a Lightning node ' +
      'that moved. Updating or restarting your node gives it a new address, ' +
      'and 0.6.11 taught Forktower to follow that — but only for reading your ' +
      'channels. The check that reports whether your channels are actually ' +
      'backed up to a watchtower kept dialling the old address, so the ' +
      'dashboard recovered on its own while that check went on failing in the ' +
      'log. Both now share one way of following a node, and a check in the ' +
      'build refuses any future client that cannot.',
  },
  migrations: {
    up: async ({ effects }) => {},
    down: IMPOSSIBLE,
  },
})
