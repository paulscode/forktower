import { sdk } from './sdk'
import { dashboardPort, towerPort } from './utils'

/**
 * Two interfaces: the dashboard, and the watchtower.
 *
 * Authentication on the dashboard is the platform's — the daemon runs with
 * `ui.auth = "platform"` and deliberately does not invent a second check,
 * because two passwords for one thing is not twice the safety.
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

  /**
   * The companion watchtower, which your Lightning node connects *to*.
   *
   * A raw TCP listener rather than anything the browser understands: the client
   * is LND's watchtower client speaking its own protocol, and the address here
   * is what a user pastes into `lncli wtclient add`. It carries no SSL of the
   * platform's, because the protocol does its own authenticated encryption and
   * wrapping it in another layer would produce an address LND cannot dial.
   *
   * **It is masked.** The address contains the tower's identity key, and
   * publishing that on a page anyone glancing at the screen can read tells them
   * this machine is defending Lightning channels across a split. The user
   * reveals it when they are ready to paste it.
   */
  const towerMulti = sdk.MultiHost.of(effects, 'tower')
  const towerOrigin = await towerMulti.bindPort(towerPort, {
    protocol: null,
    preferredExternalPort: towerPort,
    addSsl: null,
    secure: { ssl: false },
  })
  const tower = sdk.createInterface(effects, {
    name: 'Watchtower',
    id: 'watchtower',
    description:
      'Register this address with your Lightning node so a breach on the ' +
      'other chain gets answered rather than only reported.',
    type: 'p2p',
    schemeOverride: null,
    masked: true,
    username: null,
    path: '',
    query: {},
  })

  return [await uiMultiOrigin.export([ui]), await towerOrigin.export([tower])]
})
