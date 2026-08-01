import { sdk } from './sdk'
import { dashboardPort } from './utils'

/**
 * One interface: the dashboard.
 *
 * Authentication is the platform's — the daemon runs with `ui.auth = "platform"`
 * and deliberately does not invent a second check, because two passwords for one
 * thing is not twice the safety.
 */
export const setInterfaces = sdk.setupInterfaces(async ({ effects }) => {
  const uiMulti = sdk.MultiHost.of(effects, 'main')
  const uiMultiOrigin = await uiMulti.bindPort(dashboardPort, {
    protocol: 'http',
  })
  const ui = sdk.createInterface(effects, {
    name: 'Dashboard',
    id: 'dashboard',
    description: 'What Forktower can see, and what it means for your channels.',
    type: 'ui',
    schemeOverride: null,
    masked: false,
    username: null,
    path: '/',
    query: {},
  })

  return [await uiMultiOrigin.export([ui])]
})
