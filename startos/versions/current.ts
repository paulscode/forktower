import { IMPOSSIBLE, VersionInfo } from '@start9labs/start-sdk'
import { appVersion, packageRevision } from '../version'

export const current = VersionInfo.of({
  version: `${appVersion}:${packageRevision}`,
  releaseNotes: {
    en_US:
      'Alerts that were never separate problems are cleared away. If a ' +
      'per-block dedup fault had filled your list with thousands of copies of ' +
      'the same urgent alert, upgrading collapses them into the single entry a ' +
      'correct version would have written — and that entry closes itself as ' +
      'soon as Forktower reads a block, so there is nothing left to dismiss. ' +
      'Alerts about scanning having stopped now also say when it has started ' +
      'again, which none of them previously did. Also fixes a crash loop on ' +
      'start: a Bitcoin node loading its block index answers nothing, and ' +
      'Forktower read that as the node being on the wrong network, so it ' +
      'exited and was restarted about once a second until the node finished — ' +
      'serving no dashboard the whole time.',
  },
  migrations: {
    up: async ({ effects }) => {},
    down: IMPOSSIBLE,
  },
})
