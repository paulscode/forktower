# What Forktower defends against

Written for people who run a node, not for protocol developers. If you want the
short version of the underlying problem, read [Lightning channels and the RDTS
activation](lightning-and-the-rdts-activation.md) first.

## The situation

If the network separates into two chains, your node follows one of them. From
its point of view nothing unusual happens — it simply stops seeing certain
blocks, which looks like a quiet period.

Your Lightning channels are on both chains. Every one of them was funded by a
transaction that exists in the history both chains share, so both chains agree
your channel exists, and both will honour a transaction that spends it.

The protections on those channels are **deadlines measured in blocks**. If your
counterparty publishes an old channel state, you have a fixed number of blocks to
publish the penalty that takes their money instead. Miss it and their old state
becomes final.

Those deadlines run on both chains, independently, and your node can only see
one of them.

## The attacker this is built around

Somebody who is **already your channel counterparty**, who notices the split
before you do, and who publishes an old state on the chain your node is not
watching.

They need no special access. They do not have to break anything. They just have
to be paying attention to something you are not, and they have your channel's
old states already, because you gave the states to them yourself in the ordinary
course of the channel working.

Two properties make this worth building against:

- **It is cheap.** No hashpower, no exploit, no cooperation from anyone.
- **It is quiet.** Nothing on your node looks wrong while it happens, right up
  until the money is gone and the deadline has passed.

## What Forktower does about it

It runs a second Bitcoin node following the other chain, and watches for the
things that start a clock: a channel closing, a commitment that does not match
the current state, a sweep of a contested output. When it sees one, it tells you
what is at stake and how many blocks you have.

That is the whole idea. Everything else is detail.

## What else it defends against

**Being fed a fake quiet chain.** A second node that only ever talks to peers of
an attacker's choosing can be shown a chain where nothing is happening. Forktower
guards against that by never restricting the node to a fixed peer set — the
generated configuration uses `addnode`, never `connect`, so ordinary peer
discovery keeps running alongside anything you add. It also compares the two
views and reports when independent sources disagree about a chain's tip, rather
than trusting either.

**Watching the same chain twice.** If the second node ran software that enforces
the new rules, it would follow the same chain as your own node, agree with it
about everything, and report two chains in step while watching one of them twice.
That failure is silent, which is what makes it dangerous, so the build refuses
it: `make check` asks the bundled Bitcoin client what consensus rules it knows
and fails if any of them is the new one.

**Being told about a problem too late to act.** Deadlines are reported in blocks
rather than in hours, and escalate as they shorten. A notification that arrives
after the window has closed is not a notification, it is a report.

## What it does not defend against

Forktower **cannot close a channel for you**, and it holds nothing that could.
It reads your Lightning node with a read-only credential and there is no code in
it that could send an instruction. If Forktower is compromised, your money does
not move — the attacker gets a view of your channels, which is a real privacy
loss, and no ability to spend.

That is a deliberate trade. A tool that could act would be a tool worth stealing.

It also cannot help with:

- **states revoked before you set up a watchtower** — no watchtower covers those;
- **an honest close by either side**, which is not an attack and needs no
  response;
- **the chain your own node follows being wrong**, which is a question about your
  own node rather than about the other chain.

The full list, including the parts that are uncomfortable, is in
[Residual risks](residual-risks.md).

## What an attacker gains by compromising Forktower

Worth stating plainly, because "it only reads" is not the same as "it does not
matter".

They would learn **which channels you have, how much is in them, and who with**.
That is a map of where your money is, and for somebody choosing whom to breach it
is useful. They would also be able to **make it lie** — report calm while
something was happening — which would cost you the protection you installed it
for.

What they would not get is the ability to spend anything, sign anything, or
instruct your node to do either.

## Why the second node reaches the network through Tor

A node following only one side of a contested upgrade, from your own network
address, links the two and marks its operator as somebody with Lightning channels
they are worried about. That is targeting information for exactly the person this
software is built to protect you from.

So the second node's connections go through Tor by default, and no peer learns
your address. You can turn that off; the setting says what it costs.

## The assumption underneath all of it

**Your own node is honest and your own machine is not compromised.** Forktower
reads both, and if either is lying to it then everything it reports is downstream
of that lie. Nothing here can fix a machine you have already lost.
