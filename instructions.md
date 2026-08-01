# Forktower

Forktower watches the Bitcoin chain your own node **isn't** following, and tells
you what that means for your Lightning channels.

## Why this exists

During a contested Bitcoin upgrade the network can separate into two chains.
Your node follows one of them, and from its point of view nothing unusual has
happened — it simply stops seeing certain blocks. But your Lightning channels
exist on **both** chains, and the timers that protect them keep running on both.

A channel is protected by a deadline: if your counterparty publishes an old
state, you have a fixed number of blocks to respond before their claim becomes
final. Your node cannot respond on a chain it cannot see. It cannot even tell you
that it is happening.

Forktower runs a second Bitcoin node following the other chain, and answers three
questions your own node cannot:

- **Have the chains actually separated**, or does it only look that way?
- **Which of my channels would be exposed**, and how long do I have?
- **Is anything happening on the other chain right now** that I need to act on?

## What it does and does not do

It **reads** from your Lightning node. It holds no keys, signs nothing, and has
no code in it that could send your node an instruction. If Forktower is
compromised, your money does not move.

It **cannot close a channel for you**. What it can do is tell you, in time, that
you should — and, if you set up a watchtower and turn on transaction copying,
respond to a few specific things on the other chain automatically.

It is **useful before anything happens**, which is the point. The decision worth
making is whether to keep channels open through an activation, and that decision
has to be made beforehand.

## Setting it up

Most of it is already done. Forktower reads your Bitcoin node's address from the
platform, works out the fork's block heights from the node itself, and finds your
Lightning node if you have one installed.

**Open Settings** if you want to change how much disk the second node uses
(pruned is the default and is enough), or to add notifications to your phone.
Notifications to this server's own notification centre are on already and need
nothing.

**Then open the dashboard.** It has a readiness list that tells you what is
working and what is not, in plain words, with an action beside anything that
needs one. Work down it until everything is green.

### Two things worth doing that Forktower cannot do for you

**Turn on your Lightning node's watchtower client.** In the LND app, go to
**Actions → Watchtower → Watchtower Client Settings** and add a tower. Without
this, nobody is watching your channels while your node is blind — and the
dashboard will keep telling you so until you do.

**Read the readiness list at least once.** It is the only place that says what is
*not* protected, and that is the half most people never look at.

## What you will see

A single headline that says whether you are protected, watching, or need to act.
Underneath it: the two chains and where they diverge, the channels most at risk
with a countdown on each, and a timeline of what happened and when.

If the chains separate, Forktower will say so, on this server's notification
centre and anywhere else you configured — and it will keep saying so until you
acknowledge it.

## More

- Source and documentation: https://github.com/paulscode/forktower
- The threat this addresses, written for node runners rather than protocol
  people, is in the project's documentation under "Lightning and the RDTS
  activation".
