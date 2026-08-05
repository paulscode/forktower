# A channel is at risk

Forktower has seen something on the other chain that starts a clock against one
of your channels.

## What the countdown means

It is **blocks, not time**. The number is how many blocks remain before whoever
published can claim the money. Blocks on a chain with little hashpower arrive
slowly, which gives you more real time than the number suggests — but it is not
something to rely on, because hashpower can move.

Forktower escalates as the number falls, and tells you when it has been resolved
rather than leaving you to wonder.

## What to do

**Check tower coverage for that channel first.** The towers section names the
channels a tower is *not* backing up. If the one with the countdown is not in
that list, a watchtower holds backups for it and the response may already be in
hand — Forktower will report the justice transaction confirming if it sees it.

**If there is no coverage**, you are the response. That means publishing the
penalty transaction yourself, which needs your Lightning node and the channel
state it holds. This is the situation
[guided recovery](../residual-risks.md) exists to help with, and it is
genuinely difficult — which is the argument for setting up a watchtower before
any of this happens.

## If the countdown resolves

Forktower will say so. A resolved countdown means the contested output was swept
by the party entitled to it, or the close completed honestly. Nothing further is
needed.

## If the countdown expires

Forktower will say that too, plainly, rather than quietly dropping it. An expired
countdown means the money is likely gone. Keep the record — the Details view has
the transaction ids and heights — because it is what you will need if you pursue
it any further.
