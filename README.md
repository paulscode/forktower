<p align="center">
  <img src="logo.png" alt="Forktower" width="480">
</p>

# Forktower

**Protects Lightning channels during a chain split.**

If your Bitcoin node and your channel partner's node end up following different
chains, your Lightning node can only see one of them. Forktower watches the other
one on your behalf, tells you if a channel is being closed there, and works out
how long you have to respond.

> **Status: first release (0.6.0), and a beta.** It watches the chain your node
> cannot see, tells you which channels a split puts at risk and how long you
> have, and — with a watchtower registered — gets a breach there answered rather
> than only reported. It has been run end to end against real nodes on real
> hardware. Nobody outside this project has installed it yet, and it has not had
> a third-party security review.
>
> If you are deciding what to do about your channels before the RDTS activation,
> read [Lightning channels and the RDTS activation](docs/lightning-and-the-rdts-activation.md)
> first — some of the options available to you do not require this software.

## The problem, briefly

A Lightning channel is secured by a deadline. If your channel partner publishes
an old, already-revoked channel state, you can take the entire channel balance as
a penalty — but only within a fixed window, commonly 144 to 2016 blocks. That
threat is what keeps everyone honest, and it depends on your node *seeing* the
old state get confirmed.

Your Lightning node learns about the blockchain from your Bitcoin node, and your
Bitcoin node follows one chain. During a persistent split it cannot see the
other. A partner holding an old state can publish it on the chain you are not
watching, wait out the window there, and take the money — while on your chain the
channel still appears open and healthy. If your chain is later abandoned, you
find out after the window has closed. If it persists, nothing happened and they
lost nothing by trying. That asymmetry is the problem: during a split, publishing
a revoked state stops being a gamble and becomes a free option.

```
                              the chains separate
                                       │
  your node’s chain  ──────────────────┼───────────────────────────────────►
  you see this                         │     the channel still looks open here
                                       │
  the other chain                      └───────●───────────────────✗───────►
  you see none of it                           │                   │
                                       partner publishes    the window closes,
                                       a revoked state      balance is theirs
```

## What Forktower does about it

- **Watches the chain your node is not following** and detects spends of your
  channels' funding outputs there.
- **Works out how long you have** for each one, in time rather than block counts,
  and escalates as the window closes.
- **Watches the watchtower** that has a view of that chain — the thing that
  actually publishes the penalty transaction — and says so when it has stopped
  working, because a tower that has quietly failed looks exactly like one that
  has not. Forktower does not run or register it; it checks that your node is
  really backing up to it, and which channels are covered.
- **Copies your own transactions across**, so cooperative closes and sweeps exist
  wherever they can — including a justice transaction your node has already
  published, which then punishes the same breach on the chain nobody was
  watching. It only ever forwards bytes that already exist, signed by somebody
  else; it builds nothing.
- **Tells you, loudly**, through your node appliance's own notifications or a
  channel of your choosing — and tests that the alarm works, because an untested
  alarm is not one.

## How it works, briefly

**Forktower runs a second Bitcoin node**, following the chain your own node does
not. That is the whole trick, and it is worth knowing before you install: it is
another Bitcoin node on your machine, with its own disk and its own bandwidth.
Pruned by default, and about **31 GB** once it has caught up: 20 GB of blocks,
plus the record of unspent coins, which is around 11 GB today and is not
something pruning shrinks. The watchtower it runs adds up to 2 GB more.

It reads your Lightning node — read-only, with the credential that node already
wrote for itself — to learn which channels you have. Then it watches the other
chain for anything that starts a clock against one of them, and tells you what is
at stake and how many blocks you have.

The catch is the second node's first sync, which takes about three days from
scratch — three days during which Forktower cannot see the other chain at all.
So it offers a shortcut, on the dashboard, at the point where you would otherwise
be waiting: an 8.7 GB UTXO snapshot that brings the wait down to under an hour on
decent hardware, and a few hours on a small appliance.

It is offered rather than assumed. This is the only thing Forktower ever
downloads, nothing is fetched until you press the button, and the request
follows whatever you chose for the second node's peering — which is Tor unless
you changed it. Bitcoin Core checks the
snapshot against a hash compiled into Bitcoin Core itself and refuses anything
else, so you are not required to trust whoever hosted it — and everything below
the snapshot's height is still verified in full, in the background, afterwards.

It never holds your channel keys, your seed, or anything that can spend your
money. It cannot sign a transaction, and no part of it will ever ask you for a
recovery phrase.

Forktower takes no position on which chain is legitimate. The exposure it
addresses comes from the split itself, in either direction — it protects you on
whichever chain survives.

## Documentation

Start at [docs/index.md](docs/index.md), or go straight to what you need:

| | |
|---|---|
| [Lightning channels and the RDTS activation](docs/lightning-and-the-rdts-activation.md) | What is happening, why Lightning is a special case, and what you can do — with a deadline |
| [Installing on StartOS](docs/install-startos.md) · [on Umbrel](docs/install-umbrel.md) · [anything else](docs/deploying.md) | Getting it running |
| [Runbooks](docs/runbooks/) | What to do when a particular thing happens |
| [Residual risks](docs/residual-risks.md) | What a correct, fully configured install still cannot protect you from |
| [Threat model](docs/threat-model.md) · [Security](docs/security.md) | What it defends against, and what it is prevented from doing to you |
| [Reporting a security problem](SECURITY.md) | Please do not open a public issue |

## Building

Requires Go 1.26.5 or newer. `./scripts/dev-setup.sh` installs a pinned toolchain
with checksum verification if you would rather not do it by hand.

```sh
make build     # binaries into bin/
make test      # unit and component tests, race detector on
make lint      # static analysis and formatting
make help      # everything else
```

## Contributing

Bug reports and corrections are welcome, particularly to the user-facing
documentation — it is aimed at people making a real decision on a real deadline,
and getting it right matters more than getting it published.

This is security software for a situation that cannot be rehearsed on mainnet
before it happens. Changes come with tests, and anything touching credentials,
transaction handling, or what the user is told is reviewed on the assumption that
being wrong costs someone their money.

## Licence

MIT — see [LICENSE](LICENSE). Copyright (c) 2026 Paul Lamb.
