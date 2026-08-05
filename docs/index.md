# Forktower

Watches the Bitcoin chain your own node is not following, and tells you what that
means for your Lightning channels.

## Start here

**If you are wondering whether this affects you**, read [Lightning channels and
the RDTS activation](lightning-and-the-rdts-activation.md). It is written for
somebody who runs a node and has channels open, and it explains why Lightning is
a special case and what the deadline actually is.

The short version: if the network separates into two chains, your node follows
one of them and cannot see the other. Your channels exist on both, and the
timers protecting them run on both. A counterparty who notices before you do can
publish an old state on the side you are blind to.

## Installing

| | |
|---|---|
| [StartOS](install-startos.md) | 0.3.5.x and 0.4.x |
| [Umbrel](install-umbrel.md) | From the PaulsCode community store |
| [Anything else](deploying.md) | docker compose, or the pieces separately |

## Once it is running

| | |
|---|---|
| [Runbooks](runbooks/) | What to do when a particular thing happens |
| [Residual risks](residual-risks.md) | What a correct, fully configured install still cannot protect you from |

The dashboard's readiness list is the general answer to "is this working?" — it
says what is not, and what to do about each item. It is also the only place that
says what is *not* protected, which is the half most people never look at.

## Understanding it

| | |
|---|---|
| [Threat model](threat-model.md) | What Forktower defends against, and who the attacker is |
| [Security](security.md) | What Forktower is prevented from doing to you, and how that is enforced |
| [Reporting a problem](../SECURITY.md) | If you find something that could cost somebody money |
| [Notes for a security reviewer](security-review.md) | Where to look, what would be worst, and what has already been found |

## Contributing and building

| | |
|---|---|
| [README](../README.md) | Building from source |
| [forkbench](forkbench.md) | The test harness: two chains, real nodes, a real breach |
| [Releasing](releasing.md) | For whoever cuts releases |

## The one-paragraph version of the design

Forktower runs a second Bitcoin node following the chain your own node does not.
It reads your Lightning node — read-only, with a credential that cannot move
money — to learn which channels you have. When something happens on the other
chain that starts a clock against one of them, it tells you what is at stake and
how many blocks you have. It can also watch a watchtower on that chain, and copy
your own closing transactions across.

It cannot close a channel for you and holds nothing that could. If Forktower is
compromised, your money does not move.
