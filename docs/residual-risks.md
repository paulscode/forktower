# Residual risks

What a correctly installed, correctly configured Forktower **cannot** protect you
against. This page exists because the decision to keep Lightning channels open
through a contentious activation deserves real numbers rather than reassurance,
and because a security tool that only advertises its strengths is not one you
should trust.

## The design in one paragraph

Forktower never holds your channel keys, seed, or any spend-capable credential.
It watches the chain your Bitcoin node is not following, detects spends of your
channels' funding outputs there, and computes how long you have before a delay
expires. What actually publishes a penalty on that chain is a watchtower you have
registered — which holds pre-signed penalty data, not keys — and Forktower's part
is to watch that tower closely enough to tell you when it has stopped doing its
job.

It is an observer, an alarm, and a courier: the only bytes it ever broadcasts are
bytes somebody else already signed, copied from one chain to the other unchanged.
That design is why the limits below exist: most of them are the price of not
being able to spend your money.

## Limits that come from the watchtower model

**States revoked before you registered with the tower are not covered.** A
watchtower can only publish penalty data it was given. It receives that data as
your channel state advances *after* registration. If a partner publishes a state
that was revoked before your tower knew about your channel, the tower has nothing
to publish. Installing and registering early is not a nice-to-have; it is the
whole mechanism.

**A tower that stops receiving backups is a tower that will not fire.** This is
the classic watchtower failure mode, and it is silent by nature. Forktower
monitors backup freshness and alerts on regression, but monitoring reduces the
window rather than closing it.

**A penalty transaction still has to confirm.** Fee conditions on the two chains
can differ sharply. A penalty transaction with a fee negotiated for one
environment may sit unconfirmed in the other while the delay runs out. Forktower
cannot re-sign or fee-bump it — that would require your keys. It can tell you it
is stuck, and it will.

## Limits that come from watching a chain we do not control

**If we cannot see the chain honestly, we cannot defend it.** Forktower's view
comes from peers. An attacker who controls every peer we reach can show us a
quiet chain while the real one carries the theft. Cross-checks against
independent sources, and alerts when the view looks stalled or inconsistent,
raise the cost of that attack. They do not eliminate it. A node that is fully
eclipsed is undefended, and this is true of a fully validating node too.

**If there are no reachable honest peers on the other chain, there is nothing to
watch.** A branch with effectively no reachable peers is a blind spot regardless
of software.

**Detection is not prevention.** Forktower can tell you a channel has been
breached and how many blocks remain. If no tower is registered for that channel,
that alert is a countdown, not a defence — useful, but only if you can act on it.

## Limits on what we can tell about a transaction

**We usually cannot tell a revoked commitment from a legitimate one.** Without
channel keys, a commitment transaction spending your funding output looks the
same whether your partner is cheating or force-closing honestly. Forktower treats
the ambiguous case as hostile and alerts accordingly. This means **false alarms
are expected**, and we would rather wake you unnecessarily than stay quiet during
a real breach.

**Channel types differ.** Tower support for some channel types depends on your
Lightning implementation's version. Where a channel cannot be covered, Forktower
says so per channel rather than implying blanket protection.

## Limits that are simply not our problem to solve

**Your own node's compromise.** If an attacker controls your Lightning node, no
external watcher saves you.

**A partner who cheats on the chain you are following.** Your own Lightning node
already handles that case, and handles it better than we could.

**Recovering funds that require your signature.** Some situations — sweeping your
balance from a partner's close on the other chain, for example — need a signed
transaction. Forktower will guide you through it with established tools, but it
will never ask for your seed, and no part of it will ever accept one. If any
version of this software ever presents you with a box asking for your recovery
phrase, it is not our software.

## Honest uncertainty

This tool defends against a scenario that has not happened yet, and it cannot be
tested against the real event before the real event. It is validated against a
simulated split in a controlled environment, which reproduces the mechanics we
know about. The gap between that and reality is genuine and we would rather name
it than paper over it.

If you are weighing whether to keep channels open through an activation, treat
Forktower as one input, not as a reason to relax. Closing a channel you would
have been anxious about is a complete solution; software is a partial one.
