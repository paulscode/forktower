# Notes for a security reviewer

If you have been asked to review Forktower, or have decided to, this is what I
would want to know in your position.

Start with the [threat model](threat-model.md) — it names the attacker rather
than describing a category — and [security](security.md), which is what Forktower
is prevented from doing to the user and how those limits are enforced.

## The one-sentence version

Forktower runs a second Bitcoin node following the chain the user's own node
does not, reads their Lightning node read-only to learn what channels they have,
and tells them when something on that other chain starts a clock against one.

## What would be worst

Ranked, because it shapes where attention is worth spending:

1. **Making it lie in the reassuring direction.** Reporting the chains in step
   when they have separated, or a channel safe when a countdown is running. This
   is worse than a crash: a crash is visible, and this is not. Anything that
   causes silent confidence is the most valuable finding you can bring.
2. **A credential escaping.** Anything that could move money is not supposed to
   be reachable from this process at all — see below — so a path where one is
   would be a design failure rather than a bug.
3. **Acting on somebody else's instructions.** The transaction-mirroring feature
   moves bytes between chains. Being made to move a transaction the user did not
   author would manufacture exposure that did not exist.
4. Everything else.

## Where to look

| Area | Why it is interesting |
|---|---|
| `internal/responder/mirror/policy.go` | Decides whether a transaction may be copied to the other chain. An allowlist with deny as the default case. If you can find an input that gets a counterparty's transaction moved, that is the best finding in the repository |
| `internal/sentinel/` | Decides whether the chains have really separated. Pure logic; a false negative here is silent |
| `internal/api/auth.go` | Three authentication modes, one of which deliberately serves unauthenticated behind a platform proxy |
| `internal/redact/` | Removes credentials from anything stored, logged or shown. See the note below about where it was *not* applied |
| `internal/registry/lnd/`, `internal/registry/cln/` | The only code that talks to the user's Lightning node. It should be structurally incapable of writing |
| `docker_entrypoint*.sh` | Renders every deployment's configuration. Shell, and therefore worth a careful read |

## Things the code will not let itself do

These are enforced by the linter per file, and `make check` fails if one stops
holding. Worth knowing so you can test whether the enforcement actually bites:

- The decision-making code — has the chain split, is a channel covered, may this
  transaction be copied, how long is left — cannot open a socket, read a file,
  touch the database, or read a clock. It takes facts and returns verdicts.
- The bundled second Bitcoin node cannot be a client that enforces the new
  consensus rules. `scripts/check-no-rdts.sh` asks the binary and fails the build
  otherwise. It has a self-test, so you can see it catch something.
- No transaction is constructed or signed anywhere. The mirror moves bytes that
  already exist.

## Scope changes since this review was first planned

Two things named in the original scope no longer exist, and it would waste your
time to look for them:

- **The macaroon bake path is gone.** The plan assumed Forktower would read an
  admin macaroon once in order to mint a restricted one. It turned out LND
  already writes `readonly.macaroon` at wallet creation on all three target
  platforms, and it answers every call Forktower makes. There is no bake, and no
  code reads an admin credential.
- **The signed anchor-peer list is gone.** Built, then removed when the premise
  did not survive the signalling numbers. Its implementation is in the history at
  `749681f` if you want to look at what was removed rather than what is there.

## What I found reviewing it myself

Told to you deliberately, so you can spend your time on things I did not find:

- **A node's password reached the dashboard.** Writing an RPC address as
  `http://user:pass@host` is natural, Go's HTTP client echoes the request URL
  into its errors, and that string travelled to the dashboard and into any
  support bundle. The redaction package existed and was good; it was simply not
  applied on that path. Fixed at the choke point and again at the API boundary.
- **`platform` authentication said nothing when nothing could confirm the
  proxy.** It serves unauthenticated on a non-loopback address, trusting a proxy
  is in front. On StartOS and Umbrel one is. Elsewhere — most likely because
  somebody copied a packaged configuration — nothing checked, and nothing said
  so.
- **Three anchors in the dashboard pointed at no element**, so actions telling a
  user to go and look at something scrolled them nowhere. Not a security bug, but
  a good illustration of the class: a check existed for element ids the script
  reaches for, and href targets are reached by the browser instead.

## Running it

The test harness stands up two real chains, two real Lightning nodes, and a real
breach:

```
make build
bin/forkbench up
bin/forkbench ln-up
bin/forkbench pay -times 3
bin/forkbench snapshot-mallory
bin/forkbench pay -times 3
bin/forkbench split
bin/forkbench breach -branch sq
```

That is the scenario the whole program exists for: a counterparty publishing a
revoked state on the chain the user cannot see. [forkbench](forkbench.md)
documents the rest of it, including how to reorganise a chain under the daemon
and how to stop a node mid-write.

`make integration` runs the full scenario suite. It needs Docker and takes about
ten minutes.

## What is already known

[Residual risks](residual-risks.md) lists the limits that are inherent rather
than bugs. If something you find is in there, it is not a vulnerability — but if
you think one of them is worse than that document admits, that is worth telling
me, and it is the kind of thing an outside reader is much better placed to judge
than I am.

## Reporting

[SECURITY.md](../SECURITY.md). Please do not open a public issue for anything
that could cost somebody money.

Findings that are accepted get a fix and a regression test in the same change.
Findings that are not fixed get written into the residual-risks document, named,
rather than quietly closed.
