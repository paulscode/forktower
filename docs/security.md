# How Forktower is built to be safe

[Threat model](threat-model.md) is about what Forktower defends you *from*. This
is about what Forktower is prevented from doing to you, and how those limits are
enforced rather than merely intended.

To report a problem, see [SECURITY.md](../SECURITY.md).

## It cannot spend your money

Forktower reads your Lightning node. It never writes to it, and there is no code
in it that could.

The credential it uses is the **read-only macaroon your node wrote for itself**
when the wallet was created — not an admin credential, and not one Forktower
minted. On StartOS and Umbrel the packaging hands it exactly that file. Nothing
in the daemon asks for more.

If you are running it yourself, give it `readonly.macaroon` or a rune restricted
to reading. A wider credential works and the dashboard says so rather than
refusing — being watched with more authority than necessary beats not being
watched — but the narrow one is the point.

## It is not given your seed

This one is worth spelling out because the obvious implementation gets it wrong.

On StartOS, a Lightning node's data volume contains its wallet **seed words and
password in plain text**, alongside the credentials an app actually needs. The
documented way for one app to read another's credentials is to mount that whole
volume read-only — which would hand Forktower the keys to the wallet, by mount,
which no amount of care about macaroons afterwards can undo.

So it does not do that. On StartOS 0.3.5.1 it mounts only the `public`
subdirectory, which holds the certificate and the read-only macaroon and nothing
else. On 0.4.x, where that directory is empty, a short-lived container copies
those two files out and is destroyed — the long-running daemon, the one serving a
web interface for the rest of the installation's life, never has the seed within
reach. On Umbrel the two files are bound individually rather than the directory
holding them.

## It does not fetch anything

There is no update check, no telemetry, no analytics, and no code path that
downloads configuration. A program that downloads its own configuration has
configuration belonging to whoever holds the other end of that connection.

Forktower talks to: your Bitcoin node, its own second Bitcoin node, your
Lightning node if you configured one, and whatever notification transports you
set up. That is the complete list.

## The limits are enforced, not just intended

Good intentions decay. These are checked by the build, and `make check` fails if
any of them stops being true:

**The bundled second node cannot enforce the new rules.** It is asked what
consensus rules it knows about, and the build fails if any of them is RDTS. A
client that enforced them would follow the same chain as your own node and report
two chains in step while watching one twice — a failure that looks like
everything working.

**The decision-making code cannot perform I/O.** The parts that decide whether
the chains have separated, whether a channel is covered, whether a transaction
may be copied, and how long you have are forbidden from opening a socket, reading
a file, touching the database, or reading a clock. They take facts and return
verdicts. This is enforced by the linter, per file, with a reason attached to
each rule — so the question "could this have been influenced by something other
than its inputs?" is answerable by reading one screen.

**Credentials never reach the logs or the dashboard.** Log output is visible in
the interface, so anything that could carry a secret is filtered before it gets
there.

**No transaction is ever constructed or signed.** The transaction-copying feature
takes bytes that already exist on one chain and offers those same bytes to the
other. It does not build anything. The only place the code touches a transaction
structure is to *read* one.

## The release is signed, and the build refuses to pretend otherwise

Release artefacts are checksummed and the checksums are signed on a machine that
is not connected to anything. There is no automated signing step and there cannot
be one — the key is not on the build host.

`make verify-release` refuses to pass a release with no signature beside it. That
check exists because a sibling project once shipped a single version unsigned
while every release around it was signed, and nobody noticed — an unsigned
directory looks exactly like a signed one until you check.

What is inside each release, including every third-party binary with its pinned
version and checksum, is listed in the `SBOM.md` published beside it.

## Reproducing the build

The daemon is built with `CGO_ENABLED=0`, trimmed paths and no build id, so the
same source should produce the same binary. The third-party binaries in the image
— Bitcoin Core, s6-overlay, yq — are pinned by version and verified against
checksums committed in this repository, never against a checksum fetched
alongside the download.

## What this does not protect you from

If your machine is already compromised, none of the above helps. Forktower reads
your nodes and believes what they tell it, and an attacker who controls those has
already won before Forktower is involved.
