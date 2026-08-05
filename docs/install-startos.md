# Installing on StartOS

Forktower runs on StartOS 0.3.5.x and 0.4.x. The package works the same on both;
what differs is where the settings live.

## Before you start

You need **a Bitcoin node** — the Bitcoin app, whichever implementation you run.
Forktower compares the chain it follows against the other one, so without it
there is nothing to compare.

**A Lightning node is optional.** Without one, Forktower still watches both
chains and tells you when they separate. With one, it can also tell you *which of
your channels* would be exposed and how long you have — which is the part most
people install it for.

Expect the second Bitcoin node to use around **20 GB of disk** and the bandwidth
of an initial sync. It is pruned by default, which is enough for everything
Forktower does.

## Install

Download the `.s9pk` for your version:

- StartOS 0.4.x — `forktower-040.s9pk`
- StartOS 0.3.5.x — `forktower-0351.s9pk`

Then **System → Sideload Service**, and choose the file.

Verify the download first if you want to: each release publishes `SHA256SUMS`
and a signature over it. See [Releasing](releasing.md) for what that covers.

## First start

Start it. Two health checks appear:

- **Dashboard** — should go green quickly.
- **Other chain** — reports how far the second Bitcoin node has caught up. This
  takes hours, sometimes considerably longer. Amber here is not a fault, and the
  dashboard is usable while you wait.

Forktower does not need configuring to start. The defaults are the ones most
people want.

## Settings

On **0.4.x**: the app's **Actions → Settings**.
On **0.3.5.x**: the app's **Config**.

Both offer the same things:

| | |
|---|---|
| Second node storage | Pruned (recommended), Full, or Blocks only |
| Pruned size | 20 GB by default |
| Reach the other chain directly | Off. Forktower uses Tor, because the traffic says which side of a contested upgrade you are watching |
| Talk only to onion peers | Off, and off is right for almost everybody — see below |
| Extra peers | Normally blank |
| Notifications | ntfy and webhook, both optional |

**On the onion-only setting**: Forktower reaches the network through Tor either
way, so no peer ever learns your address. Turning this on *additionally* refuses
to speak to anything but onion nodes, which leaves the second node very few
places to start from — expect a first sync measured in days rather than hours,
and few peers afterwards. It is there for people who want it, and it is not the
default for good reason.

## The one thing Forktower cannot do for you

**Turn on your Lightning node's watchtower client.**

A watchtower is what responds to a breach while your node is not looking, and it
is the difference between Forktower telling you about a problem and something
being done about it.

- **StartOS 0.4.x**: LND → **Actions → Watchtower → Watchtower Client Settings**,
  and add a tower. The app enables the client itself once the list is not empty.
- **StartOS 0.3.5.x**: LND → **Config**, and set `wtclient.active`, then restart
  LND.

Forktower's readiness list will keep telling you this is not done until it is,
and will confirm it once it sees the setting take effect — it checks rather than
taking your word for it.

## Then

Open the dashboard and work down the readiness list until it is green. It says
what is working, what is not, and what to do about each — in plain words, with
the action next to the item.

The list is also the only place that says what is *not* protected, which is the
half most people never look at. It is worth reading once.

## If something looks wrong

See [When something looks wrong](deploying.md#when-something-looks-wrong), and
the [runbooks](runbooks/) for specific situations.
