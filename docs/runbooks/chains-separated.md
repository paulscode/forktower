# The chains have separated

Forktower is reporting a real split: the two nodes disagree about the chain, and
it has confirmed the disagreement is not a temporary reorganisation.

## If it says the chains *may* be splitting

That is a different, earlier message, and it means what it says: the two nodes are
following different blocks and have not reconciled, but Forktower has not
committed to calling it a split yet. It is deliberately said early, because you
can see a fork on any block explorer the moment there is one and a dashboard that
waits until it is certain is less use than a web page.

There is nothing to do while it says that. Under Advanced you will find the block
number they disagree about and each chain's answer for it, which you can check
against any explorer yourself. If it is real, the message below replaces it,
usually within minutes. If it was an ordinary pair of blocks found at the same
moment, it goes away on its own and Forktower says so.

## First, do nothing irreversible

There is no action that has to be taken in the next few minutes. The deadlines
that matter are measured in blocks, and Forktower is counting them for you. A
rushed force-close is itself a way to lose money.

## What to look at, in order

**1. The headline.** It says whether anything of yours is actually at risk. A
split with no channel exposure is a thing to know about, not a thing to act on.

**2. The channel list.** Anything with a countdown is a channel where somebody
has published something on the other chain and a clock is running. The number is
blocks remaining, not minutes — on a chain with little hashpower, blocks can be
slow, which works in your favour here.

**3. Whether your watchtower is covering those channels.** The towers section
gives a count of how many of your channels a tower is backing up — and **names
the ones it is not**. That is the way round that matters here: a channel listed
there is one nobody is watching for you. A channel with a countdown and *no*
entry in that list is one where something will be done automatically; one that
appears in it is where you are the response.

If the list is empty and the count is your whole channel set, everything is
covered.

## If a channel has a countdown and no coverage

That is the case worth acting on. Your options, roughly in order of preference:

- **Let your watchtower handle it** — if coverage appears, nothing else is
  needed. Check the tower section rather than assuming.
- **Close the channel cooperatively** on your own chain, if the counterparty is
  responsive. This does not help on the other chain, but it stops the situation
  getting worse.
- **Force-close** if you cannot reach them. Understand that this starts its own
  timers.

## What not to do

**Do not turn off Forktower to stop the alerts.** The alerts are the only view
you have of a chain your node cannot see.

**Do not assume the quiet chain is the safe one.** Nothing happening on a chain
you can see says nothing about the one you cannot.

## Read once, before you need it

[Lightning channels and the RDTS activation](../lightning-and-the-rdts-activation.md)
explains why the decision worth making — whether to keep channels open at all —
is one that has to be made *before* this happens.
