import { compat, types as T } from "../deps.ts";

// The settings screen, mirroring the 0.4.x package's action of the same name.
//
// Short on purpose: most of what Forktower needs it works out for itself — which
// Lightning node is installed, where the Bitcoin node is, what the fork's
// heights are (the node reports them). What is left here is genuinely a choice.
export const getConfig: T.ExpectedExports.getConfig = compat.getConfig({
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
});
