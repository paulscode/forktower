import { X509Certificate } from 'node:crypto'
import { mkdir, readdir, readFile, writeFile } from 'fs/promises'
import { configJson } from './fileModels/config.json'
import { announceNewAlerts } from './notifications'
import { sdk } from './sdk'
import {
  bitcoinNetworks,
  bitcoindHost,
  bitcoindMount,
  bitcoindRpcPort,
  bitcoindZmqRawBlockPort,
  bitcoindZmqRawTxPort,
  clnHost,
  clnRestPort,
  credentialsDir,
  dashboardPort,
  dataMount,
  lndHost,
  lndProbeMount,
  lndRestPort,
  sqPeerPort,
} from './utils'

/**
 * Which Lightning node to read, once `auto` has been resolved.
 *
 * `auto` means "whichever is installed", and that question can only be answered
 * against the platform rather than against the config file.
 */
async function resolveLightning(
  effects: Parameters<Parameters<typeof sdk.setupMain>[0]>[0]['effects'],
  choice: 'auto' | 'none' | 'lnd' | 'cln',
): Promise<'none' | 'lnd' | 'cln'> {
  if (choice !== 'auto') return choice

  const installed: string[] = await effects
    .getInstalledPackages()
    .catch(() => [] as string[])
  if (installed.includes('lnd')) return 'lnd'
  if (installed.includes('c-lightning')) return 'cln'
  // Not an error, and not something to complain about. Forktower watches both
  // chains perfectly well with no Lightning node; it simply cannot say which of
  // your channels a split would expose, and the dashboard says so itself.
  return 'none'
}

/**
 * Copy the Lightning credentials into Forktower's own volume.
 *
 * **This is a throwaway container on purpose, and the reason is the wallet
 * seed.** The LND data volume holds the seed words and the wallet password in
 * plain text — `store.json` on 0.4.x — so the long-lived container, the one
 * serving an HTTP API for the rest of the installation's life, is never given a
 * mount that contains them. A provisioning step that lives for a few hundred
 * milliseconds and reads exactly two files is.
 *
 * The obvious alternative was to mount the two files directly, which the SDK
 * types allow. **It does not work on StartOS 0.4.0.1**: a single-file dependency
 * mount fails with `mount exited with exit status: 32` for both `type: 'file'`
 * and `type: 'infer'`, an error that names neither the file nor the reason.
 * Directory mounts of the same volume work, which is how this ends up being a
 * copy rather than a mount.
 *
 * Copied on every start rather than once, so a certificate LND regenerates is
 * picked up by restarting Forktower. LND does not refresh its certificate on a
 * timer by default, so in practice this is a start-up cost and nothing more.
 *
 * Returns the paths inside Forktower's own volume and the address to dial, or
 * null if there was nothing to copy — which is not a failure. A node that is
 * installed but has never been unlocked has no macaroon yet, and saying so is
 * better than refusing to start.
 */
async function copyLndCredentials(
  effects: Parameters<Parameters<typeof sdk.setupMain>[0]>[0]['effects'],
): Promise<{ cert: string; macaroon: string; address: string } | null> {
  // Our own volume too, read-write — the credentials have to land somewhere
  // that outlives this container, and everything written here has to be written
  // *through* its rootfs. A bare path writes into the supervisor's filesystem
  // instead, where the copy appears to succeed and the daemon then cannot find
  // the file.
  const mounts = sdk.Mounts.of()
    .mountVolume({
      volumeId: 'main',
      subpath: null,
      mountpoint: dataMount,
      readonly: false,
    })
    .mountDependency({
      dependencyId: 'lnd',
      volumeId: 'main',
      subpath: null,
      mountpoint: lndProbeMount,
      readonly: true,
    })

  return sdk.SubContainer.withTemp(
    effects,
    { imageId: 'main' },
    mounts,
    'forktower-lnd-credentials',
    async (probe) => {
      const root = `${probe.rootfs}${lndProbeMount}`

      // Which network LND actually wrote its macaroons under, rather than
      // assuming mainnet — a wrong guess here reads as "your node has no
      // channels".
      const chainDir = `${root}/data/chain/bitcoin`
      const found = await readdir(chainDir).catch(() => [] as string[])
      const network =
        bitcoinNetworks.find((n) => found.includes(n)) ?? found[0] ?? null
      if (!network) return null

      const cert = await readFile(`${root}/tls.cert`).catch(() => null)
      const macaroon = await readFile(
        `${chainDir}/${network}/readonly.macaroon`,
      ).catch(() => null)
      if (!cert || !macaroon) return null

      const into = `${probe.rootfs}${credentialsDir}`
      await mkdir(into, { recursive: true })
      await writeFile(`${into}/lnd-tls.cert`, cert, { mode: 0o600 })
      await writeFile(`${into}/lnd-readonly.macaroon`, macaroon, {
        mode: 0o600,
      })
      return {
        cert: `${dataMount}/credentials/lnd-tls.cert`,
        macaroon: `${dataMount}/credentials/lnd-readonly.macaroon`,
        address: await lndAddress(probe, cert.toString()),
      }
    },
  ).catch(() => null)
}

/**
 * An address for LND that its own certificate actually vouches for.
 *
 * Forktower pins LND's self-signed certificate rather than trusting the system
 * roots, because on this network the certificate *is* the node's identity. But
 * pinning does not exempt a connection from hostname verification: the
 * certificate still has to name what is being dialled. **StartOS 0.4.x issues
 * LND a certificate whose subjectAltName holds IP addresses and no DNS names at
 * all**, so `https://lnd.startos:8080` is refused for a hostname mismatch
 * against the correct certificate — an error that reads as if the certificate
 * were wrong.
 *
 * The address the name resolves to *is* in the certificate, so dialling that
 * verifies cleanly and gives up nothing: the same certificate, the same pin, the
 * same node.
 *
 * Falls back to the hostname whenever the check is inconclusive. A guess that
 * fails loudly with a name mismatch is better than one that quietly connects
 * somewhere unintended.
 */
async function lndAddress(
  probe: { exec: (command: string[]) => Promise<{ stdout: string | Buffer }> },
  certPem: string,
): Promise<string> {
  const fallback = lndHost
  let sans = ''
  try {
    sans = new X509Certificate(certPem).subjectAltName ?? ''
  } catch {
    return fallback
  }
  if (!sans) return fallback

  const dnsNames = sans
    .split(',')
    .map((entry) => entry.trim())
    .filter((entry) => entry.startsWith('DNS:'))
    .map((entry) => entry.slice('DNS:'.length).toLowerCase())
  if (dnsNames.includes(lndHost)) return fallback

  const addresses = sans
    .split(',')
    .map((entry) => entry.trim())
    .filter((entry) => entry.startsWith('IP Address:'))
    .map((entry) => entry.slice('IP Address:'.length))
  if (addresses.length === 0) return fallback

  // Resolved inside the container, because `.startos` names mean nothing
  // outside it.
  const resolved = await probe
    .exec(['getent', 'hosts', lndHost])
    .then((r) => r.stdout.toString().trim().split(/\s+/)[0] ?? '')
    .catch(() => '')

  return addresses.includes(resolved) ? resolved : fallback
}

export const main = sdk.setupMain(async ({ effects }) => {
  const cfg = await configJson.read().const(effects)
  if (!cfg) throw new Error('config.json not found')

  const lightning = await resolveLightning(effects, cfg.lightning)
  const lndCredentials =
    lightning === 'lnd' ? await copyLndCredentials(effects) : null

  // ────────────────────────────────────────────────────────────────────────────
  // Mounts
  //
  // **Nothing from the Lightning node is mounted here.** Its credentials were
  // copied into Forktower's own volume by `copyLndCredentials` above, in a
  // container that no longer exists — because the LND volume also holds the
  // wallet seed and password in plain text, and the daemon that serves an HTTP
  // API should not be one mount away from them.
  //
  // The Bitcoin node's datadir *is* mounted, read-only, because its cookie
  // rotates whenever it restarts and a copy would go stale. That directory holds
  // no keys: on StartOS the Bitcoin package runs without a wallet, and this is
  // the same mount its other dependents already take.
  // ────────────────────────────────────────────────────────────────────────────
  const mounts = sdk.Mounts.of()
    .mountVolume({
      volumeId: 'main',
      subpath: null,
      mountpoint: dataMount,
      readonly: false,
    })
    .mountDependency({
      dependencyId: 'bitcoind',
      volumeId: 'main',
      subpath: null,
      mountpoint: bitcoindMount,
      readonly: true,
    })

  const container = await sdk.SubContainer.of(
    effects,
    { imageId: 'main' },
    mounts,
    'forktower',
  )

  // **The second Bitcoin node gets a container of its own.** Two daemons cannot
  // share one: the second tries to enter the first's namespace, finds no init
  // there, and dies with `bitcoind exited with code 2` behind a StartOS error
  // about `/proc/1/ns/pid` — neither of which mentions the actual problem. It
  // needs only Forktower's own volume; it has no business near the user's node.
  const sqContainer = await sdk.SubContainer.of(
    effects,
    { imageId: 'main' },
    sdk.Mounts.of().mountVolume({
      volumeId: 'main',
      subpath: null,
      mountpoint: dataMount,
      readonly: false,
    }),
    'forktower-sq',
  )

  // The environment the entrypoint renders `forktower.toml` from. The daemon
  // never sees these names and the user never sees the TOML.
  const env: Record<string, string> = {
    FORKTOWER_PLATFORM: 'startos',
    FORKTOWER_DATA_DIR: dataMount,
    FORKTOWER_LOG_LEVEL: cfg.logLevel,

    // The dashboard binds every interface because the platform's proxy reaches
    // it over the app network. Authentication is the platform's, declared
    // explicitly rather than left at `none` — an unauthenticated socket on a
    // non-loopback bind is how a dashboard ends up open on somebody's LAN.
    FORKTOWER_UI_LISTEN: `0.0.0.0:${dashboardPort}`,
    FORKTOWER_UI_AUTH: 'platform',

    // The user's own node.
    FORKTOWER_SF_RPC_URL: `http://${bitcoindHost}:${bitcoindRpcPort}`,
    FORKTOWER_SF_RPC_COOKIE_PATH: `${bitcoindMount}/.cookie`,
    FORKTOWER_SF_ZMQ_RAWBLOCK: `tcp://${bitcoindHost}:${bitcoindZmqRawBlockPort}`,
    FORKTOWER_SF_ZMQ_RAWTX: `tcp://${bitcoindHost}:${bitcoindZmqRawTxPort}`,

    // The second node, which this container runs itself.
    FORKTOWER_SQ_MODE: 'all-in-one',
    FORKTOWER_SQ_P2P_PORT: String(sqPeerPort),
    FORKTOWER_SQ_BLOCKSONLY: cfg.sqMode === 'blocksonly' ? '1' : '0',
    // `prune=0` is Bitcoin Core for "keep everything", which is what Full
    // means. Blocks-only is still pruned — skipping the memory pool is a
    // separate saving from not keeping the whole chain, and a user who picked
    // the lightest option did not mean to ask for 600 GB.
    FORKTOWER_SQ_PRUNE_MB: cfg.sqMode === 'full' ? '0' : String(cfg.sqPruneMb),
    // The entrypoint reads these as words, not as flags.
    FORKTOWER_SQ_CLEARNET: cfg.sqClearnet ? 'true' : 'false',
    FORKTOWER_SQ_ONION_ONLY: cfg.sqOnionOnly ? 'true' : 'false',
    FORKTOWER_SQ_EXTRA_PEERS: cfg.sqExtraPeers ?? '',

    // Tor is the platform's, and it is already there. Host and port, not a URL:
    // this becomes Bitcoin Core's `proxy=`, which does not take a scheme.
    FORKTOWER_TOR_PROXY: 'tor.startos:9050',
  }

  if (lndCredentials) {
    env.FORKTOWER_LND_REST_URL = `https://${lndCredentials.address}:${lndRestPort}`
    env.FORKTOWER_LND_TLS_PATH = lndCredentials.cert
    env.FORKTOWER_LND_MACAROON_PATH = lndCredentials.macaroon
  }
  if (lightning === 'cln') {
    // Core Lightning's rune is not a file the platform hands us — it is minted
    // by the node on request — so there is nothing to copy and nothing to
    // mount. Until the wizard can ask the node for one, a Core Lightning user
    // configures this by hand and the dashboard says so.
    env.FORKTOWER_CLN_REST_URL = `http://${clnHost}:${clnRestPort}`
  }

  // Notifications reach the user through the platform, which the daemon has no
  // path to — so the wrapper does it. The entrypoint already writes
  // `platform_notifications = true` when the platform is `startos`, so the
  // dashboard stops reporting that it has no way to contact anyone; nothing more
  // is needed here.
  if (cfg.ntfyUrl) {
    env.FORKTOWER_NTFY_URL = cfg.ntfyUrl
    if (cfg.ntfyToken) env.FORKTOWER_NTFY_TOKEN = cfg.ntfyToken
  }
  if (cfg.webhookUrl) env.FORKTOWER_WEBHOOK_URL = cfg.webhookUrl

  // Render the configuration files once, before anything is supervised.
  //
  // The same script the compose and Umbrel deployments run, in its render-only
  // mode. One renderer for three deployments: what is tested in one is tested in
  // all of them, and a settings bug cannot be platform-specific.
  const rendered = await container.exec(
    ['/usr/local/bin/docker_entrypoint_040.sh'],
    { env },
  )
  if (rendered.exitCode !== 0) {
    throw new Error(
      `Forktower could not write its configuration: ${rendered.stderr}`,
    )
  }

  return (
    sdk.Daemons.of(effects)
      // Started first, and the dashboard waits for it — but only for it to
      // *answer*, not to finish syncing. Forktower refuses to start until it
      // can confirm which chain the second node is on, and that check is worth
      // keeping: a second node quietly following the same chain as the first
      // would agree with it about everything and protect nobody.
      .addDaemon('sq-bitcoind', {
        subcontainer: sqContainer,
        exec: {
          command: [
            '/usr/local/bin/bitcoind',
            `-conf=${dataMount}/sq/bitcoin.conf`,
            `-datadir=${dataMount}/sq`,
          ],
          env,
          runAsInit: true,
          // A Bitcoin node asked to stop mid-write needs time to finish.
          // Killing it early is how a datadir comes back needing a reindex.
          sigtermTimeout: 300_000,
        },
        ready: {
          // Ready means answering, not synced. Syncing a chain takes hours and
          // a package that will not come up until it finishes is one the user
          // cannot look at while they wait — which is exactly when they want to.
          display: 'Other chain',
          fn: async () => {
            const res = await sqContainer
              .exec([
                '/usr/local/bin/bitcoin-cli',
                `-conf=${dataMount}/sq/bitcoin.conf`,
                `-datadir=${dataMount}/sq`,
                'getblockchaininfo',
              ])
              .catch(() => null)
            if (!res || res.exitCode !== 0) {
              return {
                result: 'starting',
                message: 'The second Bitcoin node is starting',
              }
            }
            try {
              const info = JSON.parse(res.stdout.toString())
              const progress = Number(info.verificationprogress ?? 0)
              return {
                result: 'success',
                message:
                  progress >= 0.9999
                    ? `Following the other chain at block ${info.blocks}`
                    : `Answering — ${(progress * 100).toFixed(1)}% caught up`,
              }
            } catch {
              return {
                result: 'starting',
                message: 'The second Bitcoin node is starting',
              }
            }
          },
          // Opening a datadir and binding a port, not syncing a chain.
          gracePeriod: 120_000,
        },
        requires: [],
      })
      .addDaemon('main', {
        subcontainer: container,
        exec: {
          // **Runs as the container's root, unlike the compose deployment.**
          // Not a preference: the Bitcoin node's RPC cookie is mode 0600 owned
          // by root, and StartOS's Bitcoin package offers no username and
          // password to use instead — cookie authentication is the only way in.
          // Dropping to an unprivileged user here buys nothing and costs the
          // connection to the user's own node, which is the one thing Forktower
          // cannot do without.
          //
          // The uid is not the boundary on this platform anyway. Every package
          // runs as root inside its own unprivileged, uid-mapped LXC container
          // — this "root" is uid 100000 on the host and owns nothing outside
          // the container. Where the uid does buy something, in the compose
          // deployment, the unprivileged user is still used.
          command: [
            '/usr/local/bin/forktowerd',
            '--config',
            `${dataMount}/forktower.toml`,
          ],
          env,
          runAsInit: true,
          sigtermTimeout: 30_000,
        },
        ready: {
          display: 'Dashboard',
          fn: () =>
            sdk.healthCheck.checkPortListening(effects, dashboardPort, {
              successMessage: 'The dashboard is ready',
              errorMessage: 'The dashboard is starting',
            }),
        },
        requires: ['sq-bitcoind'],
      })
      .addHealthCheck('alerts', {
        // **This check is also how notifications get raised**, which is not a
        // trick: the question "is anything wrong" and the act of telling
        // somebody are the same job, and running them on one timer means the
        // platform's health dot and its notification centre can never disagree.
        ready: {
          display: 'Standing alerts',
          fn: () => announceNewAlerts(effects, container),
          gracePeriod: 60_000,
          trigger: sdk.trigger.cooldownTrigger(60_000),
        },
        requires: ['main'],
      })
  )
})
