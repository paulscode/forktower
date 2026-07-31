# The Core Lightning watchtower

Forktower runs [rust-teos](https://github.com/talaia-labs/rust-teos) — "The Eye
of Satoshi" — as the companion watchtower for Core Lightning users, with its
view of the chain pointed at the branch the user's own node is *not* following.

Its source is vendored, unmodified, at [`third_party/rust-teos`](../../third_party/rust-teos),
under the MIT licence in that directory. Copyright Talaia Labs. Nothing in it is
ours and nothing in it has been changed.

## Why it is vendored rather than fetched

Two facts, and neither is a criticism of the project:

**A Core Lightning user has no alternative.** teos implements the BOLT13 draft;
LND's watchtower speaks its own protocol. They do not interoperate, so a Core
Lightning node cannot register with an LND tower and this is the only watchtower
available to it.

**Upstream is in care-and-maintenance.** 235 commits in 2022, 70 in 2023, 24 in
2024, ten in 2025. The most recent release, `v0.2.0`, is from February 2023, and
the recent commits are drive-by work from contributors rather than the original
author. There is no archive banner and nobody has said it is finished.

The risk that follows is not that the code is broken — it builds clean today,
and its design is sound. The risk is that **if it breaks, nobody upstream will
fix it**, and we would find out at the worst possible moment. So the source is
kept here, where losing the upstream repository is an inconvenience rather than
an outage.

## Why a commit and not a tag

`v0.2.0` is three and a half years old and predates fixes we want, including
HTTPS support in the watchtower client. There will not be another release. The
exact commit is pinned in [`pinned.env`](pinned.env) and recorded again in
`third_party/rust-teos/VENDORED`, so a silent change is a visible one.

## Never run `cargo update`

The committed `Cargo.lock` is the only reason an unmaintained Rust project still
builds: dependency resolution is frozen, so the bit-rot that normally kills one
has not reached it. Regenerating it would resolve three years of dependency
drift in a single step, against a codebase nobody is maintaining.

The [`Dockerfile`](Dockerfile) builds with `cargo build --locked`, which fails
rather than succeeding if the lockfile would need to change. That is the rule in
mechanical form.

## Updating the pin

1. Read what changed upstream. This is not a routine bump: there is no release
   to read notes for, and no maintainer to have vetted it.
2. Change `TEOS_COMMIT` in [`pinned.env`](pinned.env).
3. `make vendor-teos` — refetches and rewrites `third_party/rust-teos`.
4. `make teos-image` — proves it still builds, `--locked` and all.
5. Run it against a current Core Lightning. **Upstream's own CI last ran against
   v24.11.1**, so nobody else has checked this and nobody else will.

## The plugin needs a CA bundle on the node, even over plain HTTP

Found by running this against a live Core Lightning v25.09, and worth stating
plainly because the failure gives no useful clue.

The `watchtower-client` plugin builds its HTTP client eagerly at startup, and
`reqwest` constructs a TLS stack whether or not any address it will ever talk to
uses TLS. On a node image without `ca-certificates` installed, that panics:

```
thread 'tokio-runtime-worker' panicked at reqwest-0.11.27/.../client.rs:1713:38:
Client::new(): reqwest::Error { kind: Builder, source: Normal(ErrorStack([])) }
```

The panic is on a worker thread, so the plugin does not die. It loads, reports
"Plugin watchtower client initialized", starts its retry manager, logs
"Registering in the Eye of Satoshi" — and then `registertower` **never returns**.
Everything looks like it is working except the thing you asked for.

Installing `ca-certificates` on the node fixes it immediately and registration
succeeds. Core Lightning's own images do not all ship one.

## What we own

Upstream's CI pins Core Lightning v24.11.1 and Bitcoin Core 27.0 — the last
combination anyone verified. Core Lightning has shipped several releases since,
and Forktower runs Bitcoin Core 28.0. Testing this against the versions we
actually ship against is our job, because there is nobody else to do it. That is
the accepted cost of not forking, and it is cheaper than maintaining a Rust
watchtower inside a Go project.
