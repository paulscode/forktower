import { IMPOSSIBLE, VersionInfo } from '@start9labs/start-sdk'
import { appVersion, packageRevision } from '../version'

export const current = VersionInfo.of({
  version: `${appVersion}:${packageRevision}`,
  releaseNotes: {
    en_US:
      'Fixes for Forktower describing its own state wrongly after you have ' +
      'acted on it. Nothing on the dashboard could close a warning: a ' +
      'watchtower that came back, a chain that could be seen again, or a ' +
      'notification path that started working each recorded the good news as ' +
      'a fresh copy of the bad news, and un-dismissed the warning you had ' +
      'already read — so changing a setting on your own node appeared to do ' +
      'nothing. A fresh install no longer reports that no watchtower is ' +
      'protecting your channels while Forktower\'s own watchtower is still ' +
      'starting. The setup directions now give the steps in an order that can ' +
      'actually be carried out. And the faster first sync says how long your ' +
      'node has been reading instead of showing the same sentence for half an ' +
      'hour.',
  },
  migrations: {
    up: async ({ effects }) => {},
    down: IMPOSSIBLE,
  },
})
