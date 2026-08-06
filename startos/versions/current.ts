import { IMPOSSIBLE, VersionInfo } from '@start9labs/start-sdk'
import { appVersion, packageRevision } from '../version'

export const current = VersionInfo.of({
  version: `${appVersion}:${packageRevision}`,
  releaseNotes: {
    en_US:
      'Fixes found by running 0.6.0 on real hardware. On StartOS 0.3.5.1 ' +
      'Forktower could not authenticate to your Bitcoin node at all, so it ' +
      'never started and its dashboard was never served — that is fixed. A ' +
      'second node still syncing is no longer reported as a chain that was ' +
      'replaced, which was raising a critical alert on every poll. A ' +
      'watchtower that is starting up, or whose own node is still catching ' +
      'up, is no longer described as having stopped answering. And where the ' +
      'dashboard asks you to set up notifications, it now says where to do it.',
  },
  migrations: {
    up: async ({ effects }) => {},
    down: IMPOSSIBLE,
  },
})
