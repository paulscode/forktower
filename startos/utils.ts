// Shared constants for the 0.4.x sources.
//
// Sibling StartOS services are reached at their `.startos` hostnames. Everything
// here that has a number in it is a number the daemon also knows, so changing one
// means changing both — which is why they are named rather than inlined.

/** The dashboard, inside the container. Fronted by the platform's proxy. */
export const dashboardPort = 8330

/**
 * The second Bitcoin node's peer port.
 *
 * Deliberately not 8333: the user already runs a Bitcoin node on this machine,
 * and two of them wanting the same port is a support question nobody should have
 * to ask.
 */
export const sqPeerPort = 8433

/** The user's own Bitcoin node. */
export const bitcoindHost = 'bitcoind.startos'
export const bitcoindRpcPort = 8332
export const bitcoindZmqRawBlockPort = 28332
export const bitcoindZmqRawTxPort = 28333

/**
 * The Bitcoin node's data volume, mounted read-only for one file: the cookie.
 *
 * Narrow on purpose. A Bitcoin datadir contains the wallet, and Forktower has no
 * business anywhere near it.
 */
export const bitcoindCookieMount = '/mnt/bitcoind/.cookie'

/** LND, when installed. */
export const lndHost = 'lnd.startos'
export const lndRestPort = 8080

/**
 * LND credentials, mounted as two individual read-only **files**.
 *
 * **Not the volume.** See the mount block in main.ts — the reason is the wallet
 * seed, and it is worth reading before anyone widens these.
 */
export const lndCertMount = '/mnt/lnd/tls.cert'
export const lndMacaroonMount = '/mnt/lnd/readonly.macaroon'

/** Core Lightning, when installed. */
export const clnHost = 'c-lightning.startos'
export const clnRestPort = 3010
export const clnRuneMount = '/mnt/cln/rune'

/** Where the daemon's own data lives inside the container. */
export const dataMount = '/data'

/**
 * The Bitcoin networks LND may have written its macaroon under.
 *
 * Ordered, and mainnet first: the package is for people running a real node, and
 * guessing wrong sends the daemon looking for a file that is not there.
 */
export const bitcoinNetworks = ['mainnet', 'testnet', 'signet', 'regtest'] as const

export const logLevels = ['error', 'warn', 'info', 'debug'] as const
export type LogLevel = (typeof logLevels)[number]

/** How the second node is run — the same choice doc 06 §3 calls the tier. */
export const sqModes = ['full', 'pruned', 'blocksonly'] as const
export type SqMode = (typeof sqModes)[number]

/**
 * Forktower's alert tiers, mapped onto the four levels StartOS notifications
 * have.
 *
 * `loss` and `critical` both mean money is at stake and the difference between
 * them is how certain we are — which matters in the dashboard's wording and not
 * at all in a notification's colour. Anything below `warning` is a statement
 * about the world rather than a demand on the reader.
 */
export const notificationLevels = {
  loss: 'error',
  critical: 'error',
  warning: 'warning',
  notice: 'info',
  info: 'info',
} as const

export type AlertTier = keyof typeof notificationLevels

/** One alert, as `GET /api/v1/alerts` returns it. */
export type ForktowerAlert = {
  id: number
  tier: string
  kind: string
  dedup_key: string
  subject: string
  message: string
  created_at: number
  last_raised_at: number
  acked_at: number
}
