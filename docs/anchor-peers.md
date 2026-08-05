# Anchor peers

Forktower's second Bitcoin node has to find the other chain. Anchor peers are
addresses it can start looking from.

**The shipped list is empty, on purpose.** The node finds peers the ordinary way,
which is what it would do anyway. Naming specific nodes only helps if those nodes
are still there — and a list of addresses that have gone dark is worse than no
list, because it looks like a measure that is working.

That changes if the chains actually separate. The other chain may then be harder
to reach, and knowing one good address is often the whole problem solved. So the
list can be replaced without waiting for a new release — carefully, because
**whoever controls this list controls who your second node talks to.**

## Why it is signed

A node reached only through peers somebody else chose can be shown whatever they
like. In particular it can be shown a chain where nothing is happening — no
closes, no breaches, nothing to report — while something is happening on the real
one. That is the single most useful lie anyone could tell this software, and it
is why the list is not simply a file you can edit.

So a replacement list is used only if:

1. it is signed by a key built into your copy of Forktower, and
2. its version number is **higher** than the one already in use.

The second is easy to overlook and matters as much as the first. A signed list is
not the same thing as a current list. Somebody who can hand your Forktower a file
can hand it a real, properly signed list from a year ago whose peers have all
since disappeared — every signature genuine, and your node left reaching nobody.
An older or equal version is refused for that reason.

Both checks run every time the list is read, not once when it is imported. The
file lives in Forktower's data directory, and a backup restore or anyone with
access to that machine could have changed it since.

## Where a list comes from

Two ways, and no others:

- **A Forktower update.** Each release ships a list, and updating brings it.
- **You importing one.** From the dashboard, under Advanced.

**Forktower never downloads one.** There is no background fetch and no update
check, by design: a program that downloads its own configuration has
configuration belonging to whoever holds the other end of that connection. If a
new list matters enough to install, it matters enough to be something you did.

## Importing a list

You need two things: the list, and its signature — usually `sq-anchors.txt` and
`sq-anchors.txt.sig`. They are useless apart.

Paste both into the import control under **Advanced → Peers the other chain is
reached through**. Forktower will either accept it and say what is now in use, or
refuse it and say exactly why. A refusal changes nothing: the list you were
already running stays in use.

The reasons you might see:

| Message | What happened |
|---|---|
| The signature does not match that list | The list was altered after signing, or the two files do not belong together |
| Not newer than the one in use | A rollback, refused even though properly signed — see above |
| This build has no key to check against | Your copy of Forktower was built without a signing key, so it cannot accept any list |
| Not an anchor-peer list | The file is something else |

## What the dashboard shows

Under Advanced: how many peers are in the active list, its version, whether it
came from the release or from an import, and **the fingerprint of the key your
copy trusts**.

That last one is worth actually looking at. A signature check is only worth as
much as knowing whose signature it was — being told that something was verified,
without being able to see by whom, is not much of a guarantee. Compare that
fingerprint against the one published with the list.

If a list was imported and is *not* being used, the dashboard says so and why.
It will not quietly fall back and leave you believing you are running peers you
chose.

## Adding a peer yourself

You do not need a signed list to add an address. Set `FORKTOWER_SQ_EXTRA_PEERS`
to a comma-separated list, or use the second node's settings on StartOS and
Umbrel. Those are yours, they are not signed by anyone, and they work alongside
whatever list is active.

That is the right tool if somebody you trust gives you an address during a split.
The signed list is for distributing addresses to people who have no way to check
them.
