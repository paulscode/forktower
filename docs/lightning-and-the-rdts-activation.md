# Lightning channels and the RDTS activation

**Written for node operators running Bitcoin Knots 29.3 or later with a
Lightning node attached.** It describes one specific operational risk to
Lightning channels during any persistent chain split, what you can do about it,
and by when.

This document takes no position on whether BIP-110 should activate. The risk
described here exists for anyone whose Bitcoin node and their channel
counterparty's Bitcoin node end up following different chains — in either
direction, for any reason. It is a property of how Lightning's penalty mechanism
works, not a property of this proposal.

---

## The short version

1. If you run Bitcoin Knots 29.3+, your node enforces the RDTS rules. The
   software requires you to enter `rdts` in the consensus-rules setting to start,
   so this is true whether or not you thought about it at the time.
2. From block **961,632**, your node will require every block to signal. If a
   block does not signal, your node rejects it.
3. If a meaningful share of hashrate does not signal at that point, your node and
   the wider network can follow different chains for a while.
4. **While that lasts, your Lightning node can only see the chain your Bitcoin
   node follows.** It is blind to anything happening on the other one.
5. That blindness is what creates the risk: a channel partner holding an old,
   revoked channel state can publish it on the chain you cannot see, and your
   node will not react in time to punish it.
6. The decision worth making **before block 961,632** is whether to keep your
   channels open through the activation window.

Current status and how far away this is: check your own node with
`bitcoin-cli getdeploymentinfo` and look at the `reduced_data` entry, and
`bitcoin-cli getblockcount` for the current height. Those are the authoritative
numbers for your node, and they beat anything written here.

---

## Why Lightning is a special case

Most of your coins are fine. An on-chain balance is a UTXO; a chain split does
not spend it. If the two chains later reconcile, or if one is abandoned, your
coins are still yours on whichever chain persists.

Lightning is different, because a Lightning channel is a **race with a
deadline**.

When you open a channel, you and your partner hold a sequence of signed
transactions. Each time the balance changes, the previous state is *revoked* —
both sides hand over the secret that lets the other punish them for publishing
it. That punishment is the entire security model. It works because:

- if your partner publishes a revoked state, you can take the whole channel
  balance as a penalty; and
- you have a fixed window to do it, set by `to_self_delay` — commonly 144 to
  2016 blocks.

Both halves matter. The penalty deters cheating only if you actually *see* the
cheat within the window. Your Lightning node learns about the chain from your
Bitcoin node, and your Bitcoin node follows one chain.

## What a split changes

During a persistent split, publishing a revoked state stops being a losing move
and becomes a **free option** for the person holding it:

- They publish an old state on the chain your node does not follow.
- You do not see it, so no penalty transaction is broadcast there.
- The delay expires *on that chain*, and they sweep the balance.
- On your chain, nothing happened. Your channel still looks open and healthy.

If the chain you were following is later abandoned, you discover the loss after
the window has closed. If your chain persists, the old state on the other chain
becomes irrelevant and you lost nothing. From your counterparty's point of view
that is a bet with no downside — which is what makes it worth defending against
even when your counterparty has behaved honestly for years.

**This is not an argument against activating.** It is an argument for knowing
where your channels stand before the window opens, whichever outcome you expect
and whichever you would prefer.

## What actually happens to your node at block 961,632

From 961,632 through 963,647, an enforcing node requires each block to signal.
Blocks that do not signal are rejected.

How this feels in practice depends entirely on how much hashrate signals at that
point:

- **If most hashrate signals**, blocks keep arriving normally and there is no
  meaningful split. The concern in this document mostly evaporates.
- **If little hashrate signals**, blocks arrive on your chain only as fast as the
  signalling share produces them, and the rest of the network continues on
  blocks your node will not accept.

Signalling has been low through the deployment so far — you can see the live
figure in the `statistics` block of `getdeploymentinfo` on your own node. Whether
it stays low is exactly the open question, and the mandatory-signalling window is
the mechanism intended to resolve it. Reasonable people expect different
outcomes. Plan for the case you are not expecting, because that is the one that
costs money.

The practical consequence if blocks do become slow on your chain: **your
Lightning node cannot act on-chain either.** Force-closes will not confirm
promptly, and time-sensitive operations will not progress at the pace the
protocol assumes. This is worth knowing in advance, because "I will just close
my channels if it looks bad" may not be available once the window is open.

## What you can do, in rough order of effort

**Take stock now.** For each channel: capacity, your balance, your partner's
`to_self_delay`, and whether you would care about losing it. `lncli listchannels`
or `lightning-cli listpeerchannels` gives you all of it. Most operators find the
exposure is concentrated in one or two channels.

**Decide about the risky ones before block 961,632.** The options, none of which
is obviously right:

- *Close cooperatively.* A mutual close before the window removes the exposure
  entirely: no revoked state remains publishable. It costs fees, the liquidity,
  and possibly the peer relationship. This is the only option that fully removes
  the risk.
- *Keep the channel open and accept the risk.* Entirely defensible for channels
  with partners you trust, small balances, or long `to_self_delay` windows that
  give you room to react.
- *Reduce the balance at stake* by rebalancing or paying out, keeping the channel
  open with less in it.

**Prefer partners you can talk to.** During a split, the fastest resolution to
almost any channel problem is a cooperative one. A partner who answers messages
is worth more than one with better routing.

**Do not point your production Lightning node at a different chain's Bitcoin
node** to "check" on things. Lightning nodes keep local state that assumes one
chain; switching the backend underneath a live node is a reliable way to turn a
possible loss into a certain one.

**One thing that does not help:** running an old version of Bitcoin Core or Knots
does not, by itself, put you on a particular side in a way that protects your
channels. Your exposure is determined by which chain your node follows relative
to your counterparties', not by your version string.

## Where Forktower fits

Forktower is built for exactly this problem: watch the chain your node cannot
see, detect spends of your channels' funding outputs there, compute how long you
have, and make sure a watchtower with a view of that chain is in a position to
publish the penalty transaction — and say so when it is not.

**There is a first release, and it is a beta.** Nobody outside the project has
installed it, and it has had no third-party security review. If you are reading
this before the window opens, the decisions above are the ones available to you,
they are the ones with the least that can go wrong, and none of them requires our
software. Treat Forktower as something to add to a plan, not as the plan.

Forktower is deliberately neutral about the outcome. It watches whichever chain
your node is not following, and it protects you on whichever chain survives. If
RDTS activates smoothly and the network converges, it will have been unnecessary
— which is the outcome we would all prefer.

## Getting the real numbers from your own node

```sh
# Are you enforcing? (Knots 29.3+ requires it)
bitcoin-cli getnetworkinfo | grep subversion

# Deployment state, activation height, and live signalling statistics
bitcoin-cli getdeploymentinfo

# Where the chain is right now
bitcoin-cli getblockcount

# Your channel exposure
lncli listchannels          # LND
lightning-cli listpeerchannels   # Core Lightning
```

Heights are certain; dates are not. Any date you see for a future block is an
estimate that assumes ten-minute blocks, and block times vary. Work in heights.

---

*Corrections and improvements to this document are welcome — it is aimed at
operators making a real decision on a real deadline, and getting it right matters
more than getting it published.*
