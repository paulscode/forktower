import { FileHelper, z } from '@start9labs/start-sdk'
import { sdk } from '../sdk'

/**
 * What the user chose.
 *
 * main.ts reads this and hands it to the container entrypoint as environment,
 * which renders `forktower.toml`. The daemon never sees this file, and the user
 * never sees the TOML — doc 13 is explicit that nobody should be asked to edit
 * one.
 *
 * Every field has a `.catch()`, so a config written by an older version of the
 * package still loads and simply falls back for whatever it does not carry. A
 * service that refuses to start because its own settings file predates an
 * upgrade is a service that fails at exactly the wrong moment.
 */
export const configJson = FileHelper.json(
  {
    base: sdk.volumes.main,
    subpath: '/config.json',
  },
  z.object({
    // How the second Bitcoin node is run. `pruned` is the default because most
    // of the people this is for are running on a box that already holds one
    // full chain, and asking for a second is asking most of them to decline.
    sqMode: z.enum(['full', 'pruned', 'blocksonly']).catch('pruned'),
    sqPruneMb: z.number().int().catch(20_000),
    // Tor by default, in both directions. The second node's traffic says which
    // side of a contested upgrade you are watching, which is not something to
    // announce to your ISP.
    sqClearnet: z.boolean().catch(false),
    // Onion peers only. Off by default: everything already goes through Tor,
    // and refusing non-onion peers on top of that cripples a cold start.
    sqOnionOnly: z.boolean().catch(false),
    // Peers on the other chain, one per line. Empty is normal: the built-in
    // list is the starting point and this is for when it is not enough.
    sqExtraPeers: z.string().catch(''),
    // Which Lightning node to read, or none at all. `auto` picks whichever is
    // installed, which is the answer for almost everybody.
    lightning: z.enum(['auto', 'none', 'lnd', 'cln']).catch('auto'),
    // Notifications beyond the platform's own, which are always on and need no
    // configuration.
    ntfyUrl: z.string().nullable().catch(''),
    ntfyToken: z.string().nullable().catch(''),
    webhookUrl: z.string().nullable().catch(''),
    logLevel: z.enum(['error', 'warn', 'info', 'debug']).catch('info'),
  }),
)
