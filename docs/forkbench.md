# forkbench: a chain split you can run on your laptop

Forktower watches two chains and tells you when they stop agreeing. Testing that
means having two chains that actually disagree — which does not happen on demand
in the real world, and would be an expensive way to find out your setup was
wrong.

`forkbench` builds a small world for exactly this: two Bitcoin nodes in
containers that can be made to disagree with one command, and to stay that way.

It is a development tool. There is no real money anywhere near it, which is why
its credentials are written into its compose file in plain sight.

## What you need

- Docker, with `docker compose`
- Go, to build the two commands

```
make build
```

## The demo

Four commands, start to finish.

```
make forkbench-up      # two nodes, agreeing, with 200 blocks of history
make run-dev           # Forktower, in another terminal
make forkbench-split   # the chains part ways
make forkbench-status  # what the nodes say, and what Forktower makes of it
```

Open <http://127.0.0.1:8330> once `make run-dev` is going.

The development configuration sets up no notification channel, so the dashboard
leads with that — an alarm that cannot reach anyone is the most useful thing it
can tell you before anything has gone wrong:

> **You need to do something now.**
> Forktower has no way to reach you, so you would only find out something was
> wrong by looking at this page.

Within a few seconds of `forkbench split` the same line becomes:

> **You need to do something now.**
> The chains have separated — and Forktower has no way to reach you, so you would
> only find out by looking at this page.

Add a `[[alerts.transport]]` to `deploy/forkbench/forktower.dev.toml` and it
reads what a configured installation would see:

> **Something needs a look — not urgent.**
> The chains have separated: your node and the rest of the network no longer
> agree. Forktower is watching both.

Either way the split appears in the alert list and in the timeline, and the
heights and block hashes are under **Advanced** if you want them.

**Start Forktower before you split.** It reaches its watching state by seeing the
two chains agree, so if you split first it has nothing to compare against and
will report that it is still getting set up. That is a real limitation, not a
quirk of this tool.

When you are done:

```
make forkbench-down    # removes the containers and their state
```

## Commands

| Command | What it does |
|---|---|
| `forkbench up` | Starts both nodes, connects them, and mines 200 blocks so coinbases have matured. Running it again is safe. |
| `forkbench split` | Makes the two nodes disagree, permanently. Running it again does nothing. |
| `forkbench mine -node sf -blocks 6` | Adds blocks to one chain. This is how you make a split deepen. |
| `forkbench status` | Both chains' tips, and — if one is running — what Forktower makes of them. |
| `forkbench down` | Removes the world and everything in it. |

`status` asks Forktower at `http://127.0.0.1:8330` by default. Point it elsewhere
with `-forktower`, or pass `-forktower ""` to skip it.

## How the split works

Two chains that have merely lost sight of each other are not a split. They merge
the moment they can see each other again, so a world built that way would quietly
heal in the middle of whatever you were demonstrating.

So `forkbench split` does what a rule change does: it makes one node **reject** a
block the other accepted.

1. `sq-node` mines a block. Both nodes see it.
2. The nodes are disconnected and told not to reconnect.
3. `sf-node` is told that block is invalid.
4. Each node mines its own next block.

From then on `sf-node` will not accept that block, or anything built on top of
it, however long the other chain grows. Reconnecting them does not undo it —
which is the property that makes this a useful test.

You can see the rejection in `forkbench status`, in the branch column:

```
NODE  HEIGHT  TIP                BRANCHES
sf    204     0edd12c9…ed261a7a  2 (1 rejected)
sq    204     7d05158b…dd876c3c  1
```

That rejected branch is Forktower's strongest evidence: a node saying, on its
own, that it has seen the other chain and will not have it.

## Two ways this world is not the real thing

**Both nodes report no peers.** The world has exactly two nodes, so separating
them leaves each with nobody to talk to. In a real split each side keeps its own
peers, and the dashboard would not show both views as degraded.

**Nothing is signalling anything.** These are ordinary regtest nodes. The
divergence is produced by hand rather than by two pieces of software disagreeing
about a rule, which is what would happen in the real case. What Forktower sees —
two chains, a separation point, and one node rejecting the other's blocks — is
the same either way.

## When something goes wrong

`forkbench up` says the world is ready only when both nodes hold **the same
block**, not merely the same height. Two chains at equal height that disagree is
the entire situation this world exists to produce, so it would be a poor place to
be casual about the difference.

If a command cannot find the world's definition, run it from the repository, or
set `FORKBENCH_COMPOSE` to the path of `deploy/forkbench/docker-compose.yml`.

To start completely over:

```
make forkbench-down && make forkbench-up
```
