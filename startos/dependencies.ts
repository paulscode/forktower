import { configJson } from './fileModels/config.json'
import { sdk } from './sdk'

/**
 * What has to be running.
 *
 * Bitcoin always: without the user's own chain there is no comparison to make.
 *
 * A Lightning node only when the user asked for a specific one. On `auto` —
 * the default, and what almost everybody will have — neither is required,
 * because Forktower is honestly useful without one and demanding an install
 * would be claiming otherwise. Choosing LND or Core Lightning explicitly is the
 * user saying "read that one", and then it had better be running or they will
 * wonder why their channels never appear.
 */
export const setDependencies = sdk.setupDependencies(async ({ effects }) => {
  const cfg = await configJson.read().const(effects)

  return {
    bitcoind: { kind: 'running', versionRange: '>=27:0', healthChecks: [] },
    ...(cfg?.lightning === 'lnd'
      ? { lnd: { kind: 'running', versionRange: '>=0.18:0', healthChecks: [] } }
      : {}),
    ...(cfg?.lightning === 'cln'
      ? {
          'c-lightning': {
            kind: 'running',
            versionRange: '>=24:0',
            healthChecks: [],
          },
        }
      : {}),
  }
})
