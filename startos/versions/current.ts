import { IMPOSSIBLE, VersionInfo } from '@start9labs/start-sdk'
import { appVersion, packageRevision } from '../version'

export const current = VersionInfo.of({
  version: `${appVersion}:${packageRevision}`,
  releaseNotes: {
    en_US:
      'Two corrections to 0.6.11. The Copy address button copied the whole ' +
      'line beneath it — "lncli wtclient add" and then the address — rather ' +
      'than the address alone. On StartOS that value is pasted into a settings ' +
      'field rather than a terminal, so what it put on the clipboard was ' +
      'something your node\'s form would reject. And the watchtower card said ' +
      'only that the setup steps elsewhere on the page name the right settings ' +
      'for your platform. They do, on a part of the page you may never open; ' +
      'the steps for your platform are now shown on the card itself.',
  },
  migrations: {
    up: async ({ effects }) => {},
    down: IMPOSSIBLE,
  },
})
