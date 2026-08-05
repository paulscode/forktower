# Installing on Umbrel

Forktower is in the PaulsCode community app store.

## Before you start

You need **a Bitcoin node**. Either app works — the official Bitcoin Node or
Bitcoin Knots — and Forktower detects which you have. That is why it depends on
"Bitcoin Node" rather than naming one: Bitcoin Knots declares itself an
implementation of that dependency, so Umbrel lets you satisfy it with whichever
you run, and requiring one would have excluded users of the other.

**A Lightning node is optional.** Without one, Forktower still watches both
chains. With one, it can tell you which of your channels a split would expose.

Expect the second Bitcoin node to use around **20 GB of disk** and the bandwidth
of an initial sync.

## Add the store

**App Store → ⋯ → Community App Stores**, and add:

```
https://github.com/paulscode/umbrel-store
```

Forktower appears in that store alongside the other apps there.

## Install

Install it as normal. If you run Bitcoin Knots, Umbrel will ask which app should
satisfy the Bitcoin Node dependency — choose Knots. If you only have one Bitcoin
app installed it may not ask at all.

## First start

The app takes a few minutes to settle, and the second Bitcoin node then syncs for
some hours. The dashboard is usable throughout; it will tell you the other chain
is still catching up rather than pretending otherwise.

## Settings

Umbrel apps have no settings form, so Forktower's options live in the app's
`docker-compose.yml` environment block — the same names described in
[Deploying](deploying.md). The defaults suit almost everyone; the one worth
knowing about is `FORKTOWER_SQ_EXTRA_PEERS`, if the second node struggles to find
peers.

## Notifications

**Umbrel has no notification system an app can reach**, so unlike StartOS,
Forktower cannot raise anything in the platform's own interface. There is nothing
to poll and nothing to post to.

That means **notifications are worth setting up here in a way they are not
elsewhere**. Without one, alerts appear only on Forktower's dashboard — which
works, and only helps if you happen to be looking at it. The alerts that matter
most arrive while you are not.

Set `FORKTOWER_NTFY_URL` to an ntfy topic, or `FORKTOWER_WEBHOOK_URL` to
something of your own. Forktower's readiness list reports that it has no way to
reach you until you do, and it is right to.

## The one thing Forktower cannot do for you

**Turn on your Lightning node's watchtower client.**

Lightning → **Advanced Settings** → `wtclient.active`. There is also
`wtclient.sweep-fee-rate` there, which controls what a watchtower will pay to get
a penalty confirmed; the default is 10 sat/vB, and during a busy period that may
not be enough.

Forktower checks whether this took effect rather than trusting an "I did it"
button, and keeps saying so until it sees it.

## Then

Open Forktower and work down the readiness list. It says what is working, what is
not, and what to do about each. It is also the only place that says what is *not*
protected, which is worth reading once even when everything is green.

## If something looks wrong

See [When something looks wrong](deploying.md#when-something-looks-wrong), and
the [runbooks](runbooks/).
