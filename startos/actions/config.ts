import { configJson } from '../fileModels/config.json'
import { sdk } from '../sdk'

const { InputSpec, Value } = sdk

/**
 * The settings screen.
 *
 * Short on purpose. Doc 13's fourth principle is "answer it, don't ask it", and
 * most of what Forktower needs it can work out: which Lightning node is
 * installed, where the user's Bitcoin node is, what the fork's heights are (the
 * node reports them). What is left here is genuinely a choice — how much disk to
 * spend, whether to reach the other chain over Tor, and where else to send
 * notifications.
 */
export const inputSpec = InputSpec.of({
  sqMode: Value.select({
    name: 'Second node storage',
    description:
      'Forktower runs a second Bitcoin node following the chain your own node ' +
      'does not. Pruned keeps only recent blocks and is enough for everything ' +
      'Forktower does — it is the right answer unless you have a reason. Full ' +
      'keeps the whole chain and needs several hundred gigabytes. Blocks-only ' +
      'skips the memory pool, which uses the least of everything but means ' +
      'Forktower sees a transaction on the other chain when it is mined rather ' +
      'than when it is broadcast.',
    values: {
      pruned: 'Pruned (recommended)',
      full: 'Full',
      blocksonly: 'Blocks only (lightest)',
    },
    default: 'pruned',
  }),
  towerEnabled: Value.toggle({
    name: 'Run a watchtower',
    description:
      'Runs a watchtower here, watching the other chain, and shows you the ' +
      'address to register with your Lightning node. A watchtower is what ' +
      'turns a breach on that chain from something you are told about into ' +
      'something that gets answered while you are asleep. It holds no keys ' +
      'and cannot spend anything; it stores encrypted backups your node gives ' +
      'it, which it cannot read until the moment it needs them. Costs a few ' +
      'hundred megabytes of memory and up to 2 GB of disk.',
    default: true,
  }),
  sqPruneMb: Value.number({
    name: 'Pruned size (MB)',
    description:
      'How much disk the second node may use for blocks when pruned. The ' +
      'default is comfortable; below about 5,000 MB Bitcoin Core will refuse.',
    required: true,
    default: 20_000,
    min: 5_000,
    integer: true,
    units: 'MB',
  }),
  sqClearnet: Value.toggle({
    name: 'Reach the other chain over the internet directly',
    description:
      'Off by default, and off is the better answer. Forktower reaches the ' +
      'other chain over Tor, because the traffic says which side of a ' +
      'contested upgrade you are watching and that is nobody else’s business. ' +
      'Turn this on only if Tor will not connect for you.',
    default: false,
  }),
  sqOnionOnly: Value.toggle({
    name: 'Talk only to onion peers',
    description:
      'Off by default, and off is right for almost everybody. Forktower ' +
      'already reaches the other chain through Tor either way, so no peer ever ' +
      'learns your address. Turning this on additionally refuses to speak to ' +
      'anything but onion nodes — which means your second node cannot use the ' +
      'usual seeds and has very few places to start from. Expect a first sync ' +
      'measured in days rather than hours, and few peers afterwards.',
    default: false,
  }),
  sqExtraPeers: Value.textarea({
    name: 'Extra peers on the other chain',
    description:
      'Normally leave this blank — your second node finds peers the usual ' +
      'way. Forktower ships no peer list of its own: naming nodes that later ' +
      'go dark would look like a measure that is working when it is not. If ' +
      'the other chain is hard to reach during a split, one good address here ' +
      'is often the whole problem solved. One per line.',
    placeholder: 'host:port, one per line',
    required: false,
    default: null,
  }),
  lightning: Value.select({
    name: 'Lightning node',
    description:
      'Which node Forktower reads your channels from. Leave this on ' +
      'Automatic: it uses whichever of LND or Core Lightning you have ' +
      'installed. Forktower only ever reads — it holds no keys and cannot move ' +
      'your money. With no Lightning node it still watches both chains; it ' +
      'just cannot tell you which of your channels a split would expose.',
    values: {
      auto: 'Automatic (recommended)',
      lnd: 'LND',
      cln: 'Core Lightning',
      none: 'None',
    },
    default: 'auto',
  }),
  ntfyUrl: Value.text({
    name: 'ntfy topic URL',
    description:
      'Optional, and extra. Forktower already sends notifications to this ' +
      'server’s own notification centre, which needs no setting up. Add an ' +
      'ntfy topic here to be told on your phone as well — worth it, because ' +
      'the alerts that matter most are the ones that arrive while you are not ' +
      'looking at this screen. A public server with a guessable topic name is ' +
      'a public channel.',
    placeholder: 'https://ntfy.sh/your-secret-topic-name',
    required: false,
    default: null,
  }),
  ntfyToken: Value.text({
    name: 'ntfy access token',
    description: 'Only if your ntfy server requires one.',
    required: false,
    default: null,
    masked: true,
  }),
  webhookUrl: Value.text({
    name: 'Webhook URL',
    description:
      'Optional. Forktower posts a JSON body to this address for each alert.',
    placeholder: 'https://example.com/hook',
    required: false,
    default: null,
  }),
  logLevel: Value.select({
    name: 'Log level',
    description:
      'Leave on Info unless you are chasing something. Debug is verbose.',
    values: {
      error: 'Error',
      warn: 'Warning',
      info: 'Info',
      debug: 'Debug',
    },
    default: 'info',
  }),
})

export const config = sdk.Action.withInput(
  'config',
  async ({ effects }) => ({
    name: 'Settings',
    description: 'How Forktower watches, and where it tells you.',
    warning: null,
    allowedStatuses: 'any',
    group: null,
    visibility: 'enabled',
  }),
  inputSpec,
  // Pre-fill from what is already saved, so opening the screen shows the
  // current state rather than the defaults.
  async ({ effects }) => configJson.read().const(effects),
  async ({ effects, input }) => {
    // The form gives back `null` for an empty optional field; the file model
    // wants it absent. Same meaning, different spelling, and the conversion has
    // to happen somewhere.
    await configJson.merge(effects, {
      ...input,
      sqExtraPeers: input.sqExtraPeers ?? undefined,
      ntfyUrl: input.ntfyUrl ?? undefined,
      ntfyToken: input.ntfyToken ?? undefined,
      webhookUrl: input.webhookUrl ?? undefined,
    })
  },
)
