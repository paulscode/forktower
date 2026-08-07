import { IMPOSSIBLE, VersionInfo } from '@start9labs/start-sdk'
import { appVersion, packageRevision } from '../version'

export const current = VersionInfo.of({
  version: `${appVersion}:${packageRevision}`,
  releaseNotes: {
    en_US:
      'Forktower\'s watchtower can now be reached by a Lightning node that ' +
      'routes its connections through Tor. On such a node every outbound dial ' +
      'goes through the platform\'s Tor proxy, which refuses local addresses — ' +
      'so the watchtower client\'s request never left the proxy, no session ' +
      'was ever agreed, and nothing said so: that failure is logged only at ' +
      'debug level, and a tower that can never be reached looks exactly like ' +
      'one about to succeed. Forktower now offers its tower over an onion ' +
      'address where one exists, and asks the Tor service to create one where ' +
      'it does not. Two new warnings cover what was silent before: a ' +
      'registration whose address no longer reaches the tower, and a tower ' +
      'your node cannot reach at all. The tower\'s own log level is also a ' +
      'setting now, which is what finding this needed and did not have.',
  },
  migrations: {
    up: async ({ effects }) => {},
    down: IMPOSSIBLE,
  },
})
