# Forktower is not sure

Sometimes the honest answer is that it cannot tell. Forktower says so rather than
guessing, and this is what those states mean.

## "Independent sources disagree about this chain"

Two views that should agree do not. That can mean a temporary reorganisation, or
it can mean one of the views is being fed something false.

Forktower will not resolve this for you, because it cannot: from inside, a
fabricated chain and a real one look the same. What it does is refuse to report
confidence it does not have.

**What to do**: check the Advanced section for both chains' tips and heights. If
one has stopped advancing while the other moves, the stalled one is the one to
doubt. Adding a peer you trust to the second node is the most direct remedy.

## "Catching up"

The second node has not finished syncing. Everything it says about the other
chain is incomplete rather than wrong. Wait.

## "Cannot reach your Lightning node"

Forktower keeps watching the channels it already knew about and tells you it has
lost contact. It does not forget them, and it does not pretend the last reading
is current.

**What to do**: check that the Lightning app is running. If it is, the credential
may have changed — on StartOS and Umbrel the packaging supplies it, so restarting
Forktower is usually enough to pick up a new one.

## "No way to reach you"

Forktower has no notification transport configured, so alerts appear only on the
dashboard. On StartOS the platform's own notification centre covers this. On
Umbrel there is nothing an app can reach, so this is worth fixing — see
[Installing on Umbrel](../install-umbrel.md).

## Why it says these things at all

A tool whose entire job is telling you about something you cannot see for
yourself has one thing it must never do, which is sound confident when it is not.
Every one of these states exists because the alternative was to report calm and
be wrong.
