# Running Forktower

Forktower watches two chains — the one your Bitcoin node follows, and the one it
does not — and tells you when they stop agreeing.

To do that it needs two things: read access to your node, and a second Bitcoin
node of its own. It runs the second one for you.

It never sends your node anything, never touches your wallet, and never asks for
a seed or a recovery phrase.

## What it costs

About **10 GB of disk** and a spare CPU core, for the second node. That node is
pruned and holds no wallet — it exists to answer questions about blocks.

The first sync takes a while: a day or two on a Raspberry Pi, a few hours on
anything faster. Start it before you need it.

## docker compose

```
git clone https://github.com/paulscode/forktower
cd forktower/deploy/compose
cp .env.example .env
chmod 600 .env
```

Open `.env` and fill in the first section — the address of your own Bitcoin node,
and either its cookie file or an RPC username and password. Everything else has a
working default.

```
docker compose up -d
```

Then open <http://127.0.0.1:8330>.

The dashboard answers one question at the top of the page: **am I OK?** If it
says *"Watching. Your channels look fine."* there is nothing for you to do.

### Reaching it from another machine

By default the dashboard is published to the machine it runs on and nothing else.
Serving it to your network without a password is refused rather than allowed
quietly.

To reach it from elsewhere, set a password:

```sh
htpasswd -nbBC 12 "" "the password you want" | cut -d: -f2
```

Put the result in `.env` as `FORKTOWER_UI_PASSWORD_HASH`, set
`FORKTOWER_UI_AUTH=password`, and change `FORKTOWER_UI_BIND` to `0.0.0.0`.

### Telling it how to reach you

With nothing configured, Forktower says so on its own dashboard — an alarm nobody
can hear is worth knowing about before you need it.

Set `FORKTOWER_NTFY_URL` or `FORKTOWER_WEBHOOK_URL` in `.env` and it will send
itself a test message on startup, then weekly, so you find out that the path
works while nothing is wrong.

Whatever you point it at is told **the severity and the kind of thing that
happened, and nothing else** — not which channel, not how much, not how long you
have. That detail stays on your machine. Whoever runs a notification service
would otherwise learn that you are under attack and roughly when, and there is no
version of that which helps you.

A notification server you run yourself is the private option. A public one with a
topic name someone could guess is, in effect, a public channel.

## StartOS and Umbrel

Install from the app store. The platform passes Forktower its own settings and
its own node's address, so there is nothing to configure by hand.

## Running the pieces yourself

The published image can run two ways.

`FORKTOWER_SQ_MODE=all-in-one` runs the daemon and the second Bitcoin node in one
container, supervised together. This is what the packaged apps use, because those
platforms want a single image.

`FORKTOWER_SQ_MODE=external` runs the daemon alone and expects the second node to
be its own service. This is what the compose file above does, and it is the
easier one to watch: each piece has its own logs.

If you already run a second Bitcoin node — one on a release that predates the
fork's rules — point `FORKTOWER_SQ_RPC_URL` at it and use `external` mode.
Forktower checks at startup that the two nodes really are two nodes: pointing it
at the same node twice would produce two views that agree by construction, and
every indicator would stay green forever while nothing was watched.

## The second node's peers

After a fork the peer population splits. A node following the status-quo chain
has to find peers still serving it, and one that cannot ends up looking at
nothing — which is hard to tell from "that chain has stopped" if nobody is
careful about the difference. Forktower is careful about it, and says which it
thinks is happening.

Two things help:

**Peers over Tor, by default.** A second node following only the status-quo chain
from your own network address links the two together and marks you as someone
with Lightning channels to defend — which is useful to exactly the wrong person.
Set `FORKTOWER_SQ_CLEARNET=true` only if you have decided that trade is worth it.

**Extra peers you know about.** `FORKTOWER_SQ_EXTRA_PEERS` takes a
comma-separated list. These are added to ordinary peer discovery rather than
replacing it — a node restricted to a handful of addresses has swapped one way of
being isolated for another.

## Upgrading

```
docker compose pull && docker compose up -d
```

The database and the second node's chain data are in volumes and survive. So does
anything Forktower has recorded: a split it has already seen is still recorded
after a restart, because losing that because a machine rebooted would drop the
one thing it exists to track.

## When something looks wrong

The dashboard's **What is in place** list says which checks are passing and, for
any that are not, what it means for you and what to do about it. A check that is
failing with nothing you can do about it says so plainly rather than leaving a
warning triangle unexplained.

For the logs:

```
docker compose logs -f forktower
docker compose logs -f sq-bitcoind
```

Forktower strips credentials out of anything it logs or stores, so these are safe
to share when you are asking for help.
