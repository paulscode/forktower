import { IMPOSSIBLE, VersionInfo } from '@start9labs/start-sdk'
import { appVersion, packageRevision } from '../version'

export const current = VersionInfo.of({
  version: `${appVersion}:${packageRevision}`,
  releaseNotes: {
    en_US:
      'Corrects a claim in 0.6.7 and closes two more alerts that could not ' +
      'stop being current. 0.6.7 said an upgrade would leave nothing to ' +
      'dismiss after collapsing duplicate alerts; it would in fact have left ' +
      'exactly one, permanently, because the entry could only be closed by a ' +
      'run that had itself seen the problem — and upgrading restarts the app. ' +
      'That is fixed, and an install upgrading straight from 0.6.7 will find ' +
      'it closed. A split ending now also closes the alert saying the chains ' +
      'had separated, which used to stand alongside the news that it was ' +
      'over. And unconfirmed transactions arriving faster than they can be ' +
      'read is no longer reported as trouble seeing the other chain: it costs ' +
      'early warning, not sight of the chain, and it was showing permanently ' +
      'on healthy installs whose second node had caught up.',
  },
  migrations: {
    up: async ({ effects }) => {},
    down: IMPOSSIBLE,
  },
})
