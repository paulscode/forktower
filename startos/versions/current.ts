import { IMPOSSIBLE, VersionInfo } from '@start9labs/start-sdk'
import { appVersion, packageRevision } from '../version'

export const current = VersionInfo.of({
  version: `${appVersion}:${packageRevision}`,
  releaseNotes: {
    en_US:
      'If you used the faster first sync, Forktower could report that the ' +
      'other chain had been replaced and stop watching it — repeatedly. ' +
      'Bitcoin Core keeps a second chainstate after that shortcut, validating ' +
      'history in the background, and both publish blocks on the same ' +
      'channel; Forktower was reading the background one as the chain it ' +
      'watches, and every alternation between them looked like a chain ' +
      'replacement. It now trusts your node\'s own account of which chain is ' +
      'active, and discards a scanning position recorded from the other one. ' +
      'Separately, on platforms that give an app no settings screen, having ' +
      'nowhere to send alerts is no longer presented as a setup step that ' +
      'cannot be completed.',
  },
  migrations: {
    up: async ({ effects }) => {},
    down: IMPOSSIBLE,
  },
})
