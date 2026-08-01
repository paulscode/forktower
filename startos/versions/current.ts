import { IMPOSSIBLE, VersionInfo } from '@start9labs/start-sdk'
import { appVersion, packageRevision } from '../version'

export const current = VersionInfo.of({
  version: `${appVersion}:${packageRevision}`,
  releaseNotes: {
    en_US:
      'First StartOS release. Forktower runs a second Bitcoin node following ' +
      'the chain your own node does not, tells you which of your Lightning ' +
      'channels a split would expose and how long you have, and can watch a ' +
      'watchtower on the other chain and copy your own closing transactions ' +
      'across. It reads from your Lightning node and never writes to it.',
  },
  migrations: {
    up: async ({ effects }) => {},
    down: IMPOSSIBLE,
  },
})
