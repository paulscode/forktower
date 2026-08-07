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
   *
   * **Checked on hardware, because the answer mattered.** An LND watchtower
   * accepts a session from anyone who can reach it and has no allowlist, so
   * whether declaring this publishes it was worth knowing rather than assuming.
   * On StartOS 0.4.0.1 the declaration produces a port binding and an empty
   * address list — no public gateway and no onion — so the tower is reachable
   * on the internal network and nowhere else.
   *
   * **That measurement was right and what was concluded from it was wrong.** It
   * was read as "good, internal-only is what we want", and the advertised address
   * became `forktower.startos:9911`. An empty address list is not a safe default;
   * it is a tower with no address a Tor-routed node can dial, and an lnd
   * configured to send every connection through Tor — the case that broke — can
   * reach neither a `.startos` name nor a private address, because the proxy
   * refuses both.
   *
   * So the binding stays and so does the empty list: the platform is right not to
   * publish anything on its own. What changed is that `main.ts` now asks the Tor
   * package for an onion and advertises it when there is one, which is a decision
   * the user makes rather than one the declaration makes for them.
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
