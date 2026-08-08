import { IMPOSSIBLE, VersionInfo } from '@start9labs/start-sdk'
import { appVersion, packageRevision } from '../version'

export const current = VersionInfo.of({
  version: `${appVersion}:${packageRevision}`,
  releaseNotes: {
    en_US:
      'Install this if you are watching the current fork. Forktower could fail ' +
      'to report a split that was really happening. It confirmed one only when ' +
      'both chains built several blocks past the point they separated — and a ' +
      'split is often exactly what stops one of them building. In the fork of ' +
      '8 August the user\'s own chain held one block past the separation and ' +
      'stopped while the other went on, so the dashboard reported the two as ' +
      'being on the same chain for as long as it lasted, while a block explorer ' +
      'showed otherwise. A split is now also confirmed when the separation ' +
      'simply persists, and you are warned within two minutes that one may be ' +
      'happening — a warning that confirms nothing and starts no countdown. ' +
      'The fork observed in production is replayed as a standing test.',
  },
  migrations: {
    up: async ({ effects }) => {},
    down: IMPOSSIBLE,
  },
})
