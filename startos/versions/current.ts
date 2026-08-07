import { IMPOSSIBLE, VersionInfo } from '@start9labs/start-sdk'
import { appVersion, packageRevision } from '../version'

export const current = VersionInfo.of({
  version: `${appVersion}:${packageRevision}`,
  releaseNotes: {
    en_US:
      'If you update your Lightning node, Forktower now follows it. Updating ' +
      'LND rebuilds its container on a new address, and Forktower kept ' +
      'dialling the old one — every read failing while the dashboard went on ' +
      'showing channels from the last successful read, so nothing looked wrong ' +
      'until a channel changed. Restarting Forktower fixed it; now nothing ' +
      'needs to. Also: a watchtower registration left over from a reinstall ' +
      'stops your node ever agreeing a session with Forktower\'s, and Forktower ' +
      'used to say there was nothing to do about it. It now names the tower, ' +
      'gives the exact command for your platform with the key already filled ' +
      'in, and offers help on the forum. The watchtower setup steps have been ' +
      'rewritten to match what is actually on each screen, and the address has ' +
      'a copy button that works over a plain LAN connection.',
  },
  migrations: {
    up: async ({ effects }) => {},
    down: IMPOSSIBLE,
  },
})
