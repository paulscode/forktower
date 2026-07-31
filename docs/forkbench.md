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
| `forkbench ln-up` | Adds two Lightning nodes, funds them, and opens a channel between them. Running it again is safe. |
| `forkbench ln-status` | Both Lightning nodes and their channels. |
| `forkbench pay -times 3` | Sends payments through the channel, which advances its state. |
| `forkbench snapshot-mallory` | Saves the counterparty's channel state, to be put back later. |
| `forkbench restore-mallory` | Puts it back, so the counterparty believes an old commitment is current. |
| `forkbench breach -branch sq` | Restores that state, publishes the old commitment, and puts it on one chain only. |
| `forkbench coop-close` | Closes the channel by agreement. |
| `forkbench force-close -ln-node user` | Closes it unilaterally, honestly. |

Any of the three closing commands takes `-fixtures DIR` to save the transaction
it produced, which is how the classifier's test data is made.

`status` asks Forktower at `http://127.0.0.1:8330` by default. Point it elsewhere
with `-forktower`, or pass `-forktower ""` to skip it.

## Staging the attack

`make demo-s1-detect` runs the whole thing:

```
forkbench up               two Bitcoin nodes, agreeing
forkbench ln-up            two Lightning nodes, one channel
forkbench pay -times 3     the channel advances
forkbench snapshot-mallory the counterparty's state is saved here
forkbench pay -times 3     the channel advances past it
forkbench split            the chains separate
forkbench breach -branch sq
```

That last step is the one worth understanding. The counterparty is given back the
channel database it had three payments ago, so it now believes a commitment it
has since promised never to publish is still current. It force-closes, and that
transaction is pushed onto the other chain and mined there — and only there.

Afterwards, the user's own Bitcoin node shows nothing at all. Their channel is
open, their balance is what it was, and every tool they have says everything is
fine. On the chain nobody is watching, an old commitment is confirmed and a
countdown has started.

That asymmetry is the entire reason this software exists, and the reason the
world can stage it is the reason the world exists.

**This is a staged attack against a node you control, in a throwaway regtest
world with no money in it.** The counterparty is called `mallory` so that nobody
reading a log has to wonder. Doing this to somebody else's channel would be
theft, and it would also lose you money: the punishment mechanism this
demonstrates is exactly what makes it a bad idea on a chain anybody is watching.
The whole problem is that during a split, nobody is.

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

**The counterparty is cooperating with its own downfall.** A real attacker would
not helpfully stop their node so its database could be copied. Rolling LND back
by restoring a directory is a shortcut to a state a determined counterparty would
reach some other way, and what lands on the chain afterwards is a genuine revoked
commitment either way — which is the part that matters here.

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
