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

/**
 * Where the companion watchtower listens for the user's Lightning node.
 *
 * 9911 is LND's own default for a watchtower, so somebody reading the address
 * recognises what it is, and anything else would be a number to explain.
 */
export const towerPort = 9911

/** The user's own Bitcoin node. */
export const bitcoindHost = 'bitcoind.startos'
export const bitcoindRpcPort = 8332
export const bitcoindZmqRawBlockPort = 28332
export const bitcoindZmqRawTxPort = 28333

/**
 * The Bitcoin node's datadir, mounted read-only.
 *
 * A whole directory rather than the one file wanted, because a single-file
 * dependency mount does not work on StartOS 0.4.0.1. Acceptable here and only
 * here: the platform's Bitcoin package runs without a wallet, so the directory
 * holds the chain and an RPC cookie and no keys at all. The cookie has to be
 * live — it is rewritten every time Bitcoin restarts — so it cannot be copied
 * the way the Lightning credentials are.
 */
export const bitcoindMount = '/mnt/bitcoind'

/** LND, when installed. */
export const lndHost = 'lnd.startos'
export const lndRestPort = 8080

/**
 * Where the LND volume is mounted **while its credentials are copied out**, in a
 * throwaway container that is destroyed immediately afterwards.
 *
 * The long-lived container never mounts this. See `copyLndCredentials` in
 * main.ts — the reason is the wallet seed, and it is worth reading before anyone
 * moves this mount somewhere more convenient.
 */
export const lndProbeMount = '/mnt/lnd'

/** Where the copied credentials live, inside Forktower's own volume. */
export const credentialsDir = '/data/credentials'

/** Core Lightning, when installed. */
export const clnHost = 'c-lightning.startos'
export const clnRestPort = 3010

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
