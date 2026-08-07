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
  towerHost,
  towerHostId,
  towerPort,
  torPackageId,
  addOnionAction,
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

  // The watchtower gets a subcontainer of its own, for the same reason the
  // second node does: two daemons cannot share one, because the second tries to
  // enter the first's namespace, finds no init there, and dies with an error
  // about neither of them.
  const towerContainer = await sdk.SubContainer.of(
    effects,
    { imageId: 'main' },
    sdk.Mounts.of().mountVolume({
      volumeId: 'main',
      subpath: null,
      mountpoint: dataMount,
      readonly: false,
    }),
    'forktower-tower',
  )

  // Where the user's Lightning node should dial the watchtower.
  const towerAddress = await resolveTowerAddress(effects, cfg.towerEnabled)

  // The environment the entrypoint renders `forktower.toml` from. The daemon
  // never sees these names and the user never sees the TOML.
  const env: Record<string, string> = {
    FORKTOWER_PLATFORM: 'startos-0.4',
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

    // The companion watchtower. On by default: until it existed, a packaged
    // user was told to register one and given nowhere to get one.
    FORKTOWER_TOWER_LND_ENABLED: cfg.towerEnabled ? 'true' : 'false',
    // The socket lnd binds inside the container. **Not ..._LISTEN**, which the
    // daemon reads as the address to advertise and rightly refuses as a
    // wildcard.
    FORKTOWER_TOWER_LND_BIND: `0.0.0.0:${towerPort}`,
    // The tower's own logging. The entrypoint has read this since the tower
    // existed and nothing ever set it, so the only way to see inside the tower
    // was to rebuild.
    FORKTOWER_TOWER_LND_LOG_LEVEL: cfg.towerLogLevel,
    // **The address the user's node can actually dial, which is not always the
    // sibling hostname.**
    //
    // This said `forktower.startos:9911` and argued that a sibling hostname was
    // both the obvious answer and the safer one. The topology part was right and
    // the conclusion was wrong, because it never accounted for how the client
    // gets out. Measured on StartOS 0.4.0.1: lnd running with
    // `tor.skip-proxy-for-clearnet-targets=false` sends *every* dial through the
    // platform's Tor SOCKS proxy, and Tor refuses RFC1918 targets outright and
    // cannot resolve a `.startos` name. The proxy answered `GENERAL FAILURE` and
    // the watchtower client retried in silence for eighty-seven minutes.
    //
    // So the address has to be one Tor can carry, and on this platform that is an
    // onion. With one attached, a session was agreed in under three minutes and
    // sixty-five backups went across — the first working session ever observed.
    //
    // The security point in the old comment survives and is why the onion is
    // asked for rather than assumed: an LND watchtower accepts a session from
    // anyone who can reach it and has no allowlist. The sibling hostname is kept
    // as the fallback because it is genuinely correct for a node dialling
    // directly, which is the platform's own default.
    FORKTOWER_TOWER_LND_EXTERNAL_ADDR: towerAddress,
    // The sibling hostname still reaches the tower whatever is advertised, and a
    // node registered against it before an onion existed is not to be told its
    // registration has gone bad. Listed unless it *is* what is advertised.
    FORKTOWER_TOWER_LND_ALSO_REACHABLE_AT:
      towerAddress === `${towerHost}:${towerPort}`
        ? ''
        : `${towerHost}:${towerPort}`,
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
      // The companion watchtower. Third rather than second because it needs the
      // chain backend the second node provides, and starting it first would
      // mean lnd retrying a connection that cannot succeed yet.
      .addDaemon('tower', {
        subcontainer: towerContainer,
        exec: {
          // Guarded in the shell rather than by leaving the daemon out of the
          // list. The daemon set is fixed at build time and its ids are what
          // `requires` refers to, so a conditional entry would make the
          // dashboard's dependency graph depend on a setting.
          command: [
            '/bin/sh',
            '-c',
            `if [ "$FORKTOWER_TOWER_LND_ENABLED" = "true" ]; then ` +
              `exec /usr/local/bin/lnd-tower --configfile=${dataMount}/tower/lnd.conf; ` +
              `else echo "the watchtower is switched off" >&2; exec sleep infinity; fi`,
          ],
          env,
          runAsInit: true,
          // lnd closes a bbolt database on the way out, and killing it early is
          // how that database comes back needing recovery.
          sigtermTimeout: 60_000,
        },
        ready: {
          display: 'Watchtower',
          fn: async () => {
            if (!cfg.towerEnabled) {
              return { result: 'success', message: 'Not running, by your choice' }
            }
            return sdk.healthCheck.checkPortListening(effects, towerPort, {
              errorMessage: 'The watchtower is starting',
              successMessage: 'Ready to answer a breach on the other chain',
            })
          },
          // lnd opens its databases and generates certificates on a first run,
          // which on an appliance is not quick.
          gracePeriod: 180_000,
        },
        requires: ['sq-bitcoind'],
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

/**
 * Where the user's Lightning node should dial the watchtower.
 *
 * **An onion when there is one, and the sibling hostname otherwise.** Which of
 * those works is not a property of this machine's topology but of how the
 * client's node gets out: an lnd configured to send every connection through
 * Tor — the case on StartOS 0.4.0.1 — cannot reach a `.startos` name or a
 * private address at all, because the proxy refuses both. An lnd dialling
 * directly reaches the sibling hostname fine and would have to build a Tor
 * circuit to reach an onion on the same box.
 *
 * Preferring the onion serves both: a direct-dialling node can still reach an
 * onion, and a proxied one cannot reach anything else.
 *
 * When there is no onion, this asks for one — see `askForAnOnion`. It does not
 * create one, and deliberately: a watchtower has no allowlist, so publishing one
 * is the user's decision to take.
 */
async function resolveTowerAddress(
  effects: Parameters<Parameters<typeof sdk.setupMain>[0]>[0]['effects'],
  towerEnabled: boolean,
): Promise<string> {
  const fallback = `${towerHost}:${towerPort}`
  if (!towerEnabled) return fallback

  const onion = await towerOnion(effects)
  if (onion) {
    await effects.action
      .clearTasks({ only: [`${torPackageId}:${addOnionAction}`] })
      .catch(() => null)
    return onion
  }

  await askForAnOnion(effects)
  return fallback
}

/**
 * The onion attached to the watchtower's binding, if the user has added one.
 *
 * Read from the host rather than from anything Forktower wrote down, because
 * Forktower does not create it: the Tor package attaches it as a plugin
 * hostname, and `bindings[port].addresses.available` is where those surface.
 *
 * Any failure here means "no onion known", never a thrown start. A tower on the
 * sibling hostname protects the users whose nodes can dial it; a package that
 * refuses to start protects nobody.
 */
async function towerOnion(
  effects: Parameters<Parameters<typeof sdk.setupMain>[0]>[0]['effects'],
): Promise<string | null> {
  try {
    const host = await effects.getHostInfo({ hostId: towerHostId })
    const available = host?.bindings?.[towerPort]?.addresses?.available ?? []
    for (const entry of available) {
      const hostname = entry?.hostname ?? ''
      if (!hostname.toLowerCase().endsWith('.onion')) continue
      // The port Tor publishes, which is not necessarily the port bound here —
      // the hidden service maps its own external port onto this one.
      return `${hostname}:${entry.port ?? towerPort}`
    }
  } catch {
    // No host, no bindings, or an older platform that does not answer. Nothing
    // here is worth failing a start over.
  }
  return null
}

/**
 * Ask the user to give the watchtower an onion.
 *
 * A task rather than a silent action. `effects.action.run` does take a
 * `packageId` and would create the onion outright, and it should not: an LND
 * watchtower accepts a session from anyone who can reach it and has no
 * allowlist, so putting one on the public internet is a decision to be taken
 * knowingly. The reason travels with the request so it is not a bare instruction.
 *
 * `replayId` is stable, so this is one standing request rather than a fresh one
 * every start.
 */
async function askForAnOnion(
  effects: Parameters<Parameters<typeof sdk.setupMain>[0]>[0]['effects'],
): Promise<void> {
  await effects.action
    .createTask({
      replayId: `${torPackageId}:${addOnionAction}`,
      packageId: torPackageId,
      actionId: addOnionAction,
      severity: 'important',
      reason:
        "Forktower's watchtower has no onion address, so a Lightning node " +
        'that sends its connections through Tor cannot reach it — Tor will ' +
        'not carry a connection to a local address, and the node retries in ' +
        'silence rather than reporting it. Adding an onion service to the ' +
        'Watchtower interface gives it an address any node can dial. Note that ' +
        'a watchtower accepts a session from anyone who can reach it, so this ' +
        'does publish it.',
      input: {
        kind: 'partial',
        value: {
          urlPluginMetadata: {
            packageId: 'forktower',
            hostId: towerHostId,
            internalPort: towerPort,
          },
          ssl: false,
        },
      },
    })
    .catch(() => null)
}
