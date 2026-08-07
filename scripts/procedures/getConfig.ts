import { compat, types as T } from "../deps.ts";

// The settings screen, mirroring the 0.4.x package's action of the same name.
//
// Short on purpose: most of what Forktower needs it works out for itself — which
// Lightning node is installed, where the Bitcoin node is, what the fork's
// heights are (the node reports them). What is left here is genuinely a choice.
/**
 * The settings this package accepts.
 *
 * **Exported so the migration can walk it.** StartOS checks a saved config
 * against the *current* spec when it reconfigures, and offers a missing key to
 * the spec as null — which a boolean refuses. So a config saved before a field
 * existed fails the check and the install with it. The repair belongs in a
 * migration, and a migration that has to be told the defaults by hand is a
 * migration that will be forgotten; it reads them from here instead.
 */
export const configSpec = {
  "second-node": {
    type: "object",
    name: "Second Bitcoin node",
    description:
      "Forktower runs a second Bitcoin node following the chain your own node does not.",
    spec: {
      mode: {
        type: "enum",
        name: "Storage",
        description:
          "Pruned keeps only recent blocks and is enough for everything Forktower does — the right answer unless you have a reason. Full keeps the whole chain and needs several hundred gigabytes. Blocks only skips the memory pool, which uses the least of everything but means Forktower sees a transaction on the other chain when it is mined rather than when it is broadcast.",
        values: ["pruned", "full", "blocksonly"],
        "value-names": {
          pruned: "Pruned (recommended)",
          full: "Full",
          blocksonly: "Blocks only (lightest)",
        },
        default: "pruned",
      },
      "prune-mb": {
        type: "number",
        name: "Pruned size (MB)",
        description:
          "How much disk the second node may use for blocks when pruned. Below about 5,000 MB Bitcoin Core will refuse.",
        nullable: false,
        range: "[5000,*)",
        integral: true,
        default: 20000,
        units: "MB",
      },
      clearnet: {
        type: "boolean",
        name: "Reach the other chain over the internet directly",
        description:
          "Off is the better answer. Forktower reaches the other chain over Tor, because the traffic says which side of a contested upgrade you are watching and that is nobody else's business. Turn this on only if Tor will not connect for you.",
        default: false,
      },
      "onion-only": {
        type: "boolean",
        name: "Talk only to onion peers",
        description:
          "Off is right for almost everybody. Forktower already reaches the other chain through Tor either way, so no peer ever learns your address. Turning this on additionally refuses to speak to anything but onion nodes, which leaves your second node very few places to start from — expect a first sync measured in days rather than hours, and few peers afterwards.",
        default: false,
      },
      "extra-peers": {
        type: "string",
        name: "Extra peers on the other chain",
        description:
          "Normally leave this blank — your second node finds peers the usual way. Forktower ships no peer list of its own: naming nodes that later go dark would look like a measure that is working when it is not. If the other chain is hard to reach during a split, one good address here is often the whole problem solved. Separate addresses with commas.",
        nullable: true,
        placeholder: "host:port, host:port",
      },
    },
  },
  notifications: {
    type: "object",
    name: "Notifications",
    description:
      "Where Forktower tells you, besides this server's own notification centre.",
    spec: {
      "ntfy-url": {
        type: "string",
        name: "ntfy topic URL",
        description:
          "Optional, and extra. Add an ntfy topic to be told on your phone as well — worth it, because the alerts that matter most arrive while you are not looking at this screen. A public server with a guessable topic name is a public channel.",
        nullable: true,
        placeholder: "https://ntfy.sh/your-secret-topic-name",
      },
      "ntfy-token": {
        type: "string",
        name: "ntfy access token",
        description: "Only if your ntfy server requires one.",
        nullable: true,
        masked: true,
      },
      "webhook-url": {
        type: "string",
        name: "Webhook URL",
        description:
          "Optional. Forktower posts a JSON body to this address for each alert.",
        nullable: true,
        placeholder: "https://example.com/hook",
      },
    },
  },
  // **The address the user's Lightning node dials, taken from the platform
  // rather than written down — and deliberately not nested in an object.**
  //
  // This package declares a `watchtower` interface with a `tor-config` and no
  // `lan-config`, so StartOS gives it an onion of its own and no `.local` name.
  // That address survives the container being rebuilt, which the sibling
  // hostname does not: lnd resolves the name when the tower is added and stores
  // the number, so the next rebuild leaves the registration pointing at nothing
  // with no backup arriving and nothing on the node saying so. Measured on
  // hardware, where a node sat registered at `172.18.0.18` while the tower had
  // moved to `172.18.0.24`.
  //
  // **Top-level because a new object would break every existing install.** When
  // StartOS re-runs `configure` after an update it checks the *saved* config
  // against the *new* spec, and a missing key is offered to the spec as null:
  // `ValueSpecObject::matches(Null)` is `NotNullable`, so wrapping this in an
  // object would fail that check for everybody who had configured the package
  // before — and it would need a migration to undo. A pointer matches null
  // happily and is then overwritten by the dereference, so it costs nothing and
  // needs no migration. Read from the platform source rather than guessed.
  //
  // A pointer rather than something the user types: the address is the
  // platform's to know, and asking somebody to copy their own onion into a box
  // is asking them to make a mistake.
  "watchtower-address": {
    type: "pointer",
    name: "Watchtower Tor address",
    description:
      "Where your Lightning node reaches this watchtower. Assigned by StartOS " +
      "and stable across updates, so a registration made against it does not " +
      "have to be redone.",
    subtype: "package",
    target: "tor-address",
    "package-id": "forktower",
    interface: "watchtower",
  },
  advanced: {
    type: "object",
    name: "Advanced",
    description: "Most people should leave these alone.",
    spec: {
      "log-level": {
        type: "enum",
        name: "Log level",
        description: "Leave on Info unless you are chasing something.",
        values: ["error", "warn", "info", "debug"],
        "value-names": {
          error: "Error",
          warn: "Warning",
          info: "Info",
          debug: "Debug",
        },
        default: "info",
      },
    },
  },
} as const;

export const getConfig: T.ExpectedExports.getConfig = compat.getConfig(
  configSpec as unknown as Parameters<typeof compat.getConfig>[0],
);

