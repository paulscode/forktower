import { readdir } from 'fs/promises'
import { configJson } from './fileModels/config.json'
import { announceNewAlerts } from './notifications'
import { sdk } from './sdk'
import {
  bitcoinNetworks,
  bitcoindCookieMount,
  bitcoindHost,
  bitcoindRpcPort,
  bitcoindZmqRawBlockPort,
  bitcoindZmqRawTxPort,
  clnHost,
  clnRestPort,
  clnRuneMount,
  dashboardPort,
  dataMount,
  lndCertMount,
  lndHost,
  lndMacaroonMount,
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
 * Which network subdirectory LND wrote its macaroons under.
 *
 * Discovered in a throwaway container rather than assumed, because the answer
 * decides a mount path and a wrong guess mounts nothing. `data/chain/bitcoin`
 * holds only per-network subdirectories, so this reads the least it can and
 * still get an answer.
 */
async function lndNetwork(
  effects: Parameters<Parameters<typeof sdk.setupMain>[0]>[0]['effects'],
): Promise<string | null> {
  const probeMount = '/mnt/lnd-chain'
  const mounts = sdk.Mounts.of().mountDependency({
    dependencyId: 'lnd',
    volumeId: 'main',
    subpath: '/data/chain/bitcoin',
    mountpoint: probeMount,
    readonly: true,
  })

  return sdk.SubContainer.withTemp(
    effects,
    { imageId: 'main' },
    mounts,
    'lnd-network-probe',
    async (probe) => {
      const found = await readdir(`${probe.rootfs}${probeMount}`).catch(
        () => [] as string[],
      )
      for (const net of bitcoinNetworks) {
        if (found.includes(net)) return net
      }
      return found[0] ?? null
    },
  ).catch(() => null)
}

export const main = sdk.setupMain(async ({ effects }) => {
  const cfg = await configJson.read().const(effects)
  if (!cfg) throw new Error('config.json not found')

  const lightning = await resolveLightning(effects, cfg.lightning)
  const network = lightning === 'lnd' ? await lndNetwork(effects) : null

  // ────────────────────────────────────────────────────────────────────────────
  // Mounts
  //
  // **The Lightning credentials are mounted as two individual files, and that is
  // deliberate.** The obvious thing — and what the reference packages do — is to
  // mount the LND data volume whole, read-only, and read what you need out of
  // it. Do not do that here. On both StartOS versions that volume also holds the
  // wallet's seed words and its password in plain text (`store.json` on 0.4.x,
  // `start9/cipherSeedMnemonic.txt` and `pwd.dat` on 0.3.5.1). Mounting it would
  // make a Forktower compromise a node compromise — by mount, which no amount of
  // care about macaroons afterwards can undo.
  //
  // So: `tls.cert` and `readonly.macaroon`, as files. `readonly.macaroon` is the
  // one LND itself wrote when the wallet was created, and it answers every
  // endpoint Forktower calls. Nothing needs baking and no admin credential is
  // ever read.
  //
  // If you are here to widen one of these, the question to answer first is what
  // happens to that user's coins if this daemon is compromised.
  // ────────────────────────────────────────────────────────────────────────────
  let mounts = sdk.Mounts.of()
    .mountVolume({
      volumeId: 'main',
      subpath: null,
      mountpoint: dataMount,
      readonly: false,
    })
    .mountDependency({
      dependencyId: 'bitcoind',
      volumeId: 'main',
      subpath: '/.cookie',
      mountpoint: bitcoindCookieMount,
      readonly: true,
      type: 'file',
    })

  if (lightning === 'lnd' && network) {
    mounts = mounts
      .mountDependency({
        dependencyId: 'lnd',
        volumeId: 'main',
        subpath: '/tls.cert',
        mountpoint: lndCertMount,
        readonly: true,
        type: 'file',
      })
      .mountDependency({
        dependencyId: 'lnd',
        volumeId: 'main',
        subpath: `/data/chain/bitcoin/${network}/readonly.macaroon`,
        mountpoint: lndMacaroonMount,
        readonly: true,
        type: 'file',
      })
  }

  if (lightning === 'cln') {
    mounts = mounts.mountDependency({
      dependencyId: 'c-lightning',
      volumeId: 'main',
      subpath: '/.commando-rune',
      mountpoint: clnRuneMount,
      readonly: true,
      type: 'file',
    })
  }

  const container = await sdk.SubContainer.of(
    effects,
    { imageId: 'main' },
    mounts,
    'forktower',
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
    FORKTOWER_SF_RPC_COOKIE_PATH: bitcoindCookieMount,
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
    FORKTOWER_SQ_PRUNE_MB:
      cfg.sqMode === 'full' ? '0' : String(cfg.sqPruneMb),
    // The entrypoint reads this as the word, not as a flag.
    FORKTOWER_SQ_CLEARNET: cfg.sqClearnet ? 'true' : 'false',
    FORKTOWER_SQ_EXTRA_PEERS: cfg.sqExtraPeers ?? '',

    // Tor is the platform's, and it is already there. Host and port, not a URL:
    // this becomes Bitcoin Core's `proxy=`, which does not take a scheme.
    FORKTOWER_TOR_PROXY: 'tor.startos:9050',
  }

  if (lightning === 'lnd' && network) {
    env.FORKTOWER_LND_REST_URL = `https://${lndHost}:${lndRestPort}`
    env.FORKTOWER_LND_TLS_PATH = lndCertMount
    env.FORKTOWER_LND_MACAROON_PATH = lndMacaroonMount
  }
  if (lightning === 'cln') {
    env.FORKTOWER_CLN_REST_URL = `http://${clnHost}:${clnRestPort}`
    env.FORKTOWER_CLN_RUNE_PATH = clnRuneMount
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

  return sdk.Daemons.of(effects)
    .addDaemon('main', {
      subcontainer: container,
      exec: {
        // Dropped to the unprivileged user the image creates. Nothing here
        // needs root, and a process holding privileges it does not use is a
        // process whose bugs are worth more to somebody. By full path, because
        // s6's tools are only on PATH once s6 is running and here it never is.
        command: [
          '/command/s6-setuidgid',
          'forktower',
          '/usr/local/bin/forktowerd',
          '--config',
          `${dataMount}/forktower.toml`,
        ],
        env,
        runAsInit: true,
        sigtermTimeout: 30_000,
      },
      ready: {
        // The dashboard being reachable is the blocking signal, and it must not
        // depend on the second node. A chain that is still syncing is the
        // ordinary state of a fresh install, and a package that will not come
        // up until it finishes is a package the user cannot look at while they
        // wait — which is precisely when they most want to.
        display: 'Dashboard',
        fn: () =>
          sdk.healthCheck.checkPortListening(effects, dashboardPort, {
            successMessage: 'The dashboard is ready',
            errorMessage: 'The dashboard is starting',
          }),
      },
      requires: [],
    })
    .addDaemon('sq-bitcoind', {
      subcontainer: container,
      exec: {
        command: [
          '/command/s6-setuidgid',
          'forktower',
          '/usr/local/bin/bitcoind',
          `-conf=${dataMount}/sq/bitcoin.conf`,
          `-datadir=${dataMount}/sq`,
        ],
        env,
        sigtermTimeout: 300_000,
      },
      ready: {
        display: 'Other chain',
        fn: async () => {
          const res = await container
            .exec(
              [
                '/command/s6-setuidgid',
                'forktower',
                '/usr/local/bin/bitcoin-cli',
                `-conf=${dataMount}/sq/bitcoin.conf`,
                `-datadir=${dataMount}/sq`,
                'getblockchaininfo',
              ],
              {},
            )
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
            if (progress >= 0.9999) {
              return {
                result: 'success',
                message: `Following the other chain at block ${info.blocks}`,
              }
            }
            return {
              result: 'loading',
              message: `Catching up on the other chain — ${(progress * 100).toFixed(1)}% of the way`,
            }
          } catch {
            return {
              result: 'starting',
              message: 'The second Bitcoin node is starting',
            }
          }
        },
        // Syncing a chain is measured in hours, so a slow start is not a fault.
        gracePeriod: 120_000,
      },
      requires: [],
    })
    .addHealthCheck('alerts', {
      // **This check is also how notifications get raised**, which is not a
      // trick: the question "is anything wrong" and the act of telling somebody
      // are the same job, and running them on one timer means the platform's
      // health dot and its notification centre can never disagree.
      ready: {
        display: 'Standing alerts',
        fn: () => announceNewAlerts(effects, container),
        gracePeriod: 60_000,
        trigger: sdk.trigger.cooldownTrigger(60_000),
      },
      requires: ['main'],
    })
})
