# The other chain is unreachable

Forktower is reporting that its second Bitcoin node cannot see the chain it is
supposed to be watching.

## Is it actually a problem?

**During the first sync, no.** A node catching up reports itself as syncing, and
that is not the same as unreachable. The first sync takes hours.

**Afterwards, yes.** A second node that cannot reach its chain is a blind spot,
and a blind spot that reports nothing looks exactly like a chain where nothing is
happening.

## What to check

**1. Peer count.** The Advanced section shows how many peers the second node has.
Fewer than a handful is the usual cause.

**2. Whether Tor is working.** Forktower reaches the other chain through Tor by
default. If Tor is not working on the machine, the second node has no route.
StartOS and Umbrel both run Tor themselves; check that service first.

**3. Whether you turned on onion-only.** That setting refuses every peer that is
not an onion service, which leaves the node very few places to start from. It is
the most common self-inflicted cause of this. Turn it off unless you have a
specific reason for it.

## What to do

**Add a peer you trust**, if somebody has given you an address:

```
FORKTOWER_SQ_EXTRA_PEERS=host:port
```

or the equivalent field in the app's settings. This works alongside ordinary peer
discovery — it does not replace it.

**Restart the second node** if it has been running a long time without finding
peers. On StartOS, restarting the Forktower service is enough.

## What this is not

It is not a reason to trust the readings you *do* have. If the other chain cannot
be seen, Forktower says so rather than reporting calm — and that report is the
useful part. A tool that guessed here would be worse than one that admitted it
could not tell.
