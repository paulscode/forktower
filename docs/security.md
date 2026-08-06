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

## It fetches exactly one thing, and only if you ask

There is no update check, no telemetry, no analytics, and no code path that
downloads configuration. A program that downloads its own configuration has
configuration belonging to whoever holds the other end of that connection.

Forktower talks to: your Bitcoin node, its own second Bitcoin node, your
Lightning node if you configured one, and whatever notification transports you
set up. There is one exception, and this section exists to describe it rather
than to bury it.

**The faster first sync downloads a file.** Left to itself the second Bitcoin
node takes about three days to catch up, and for those three days Forktower
cannot see the other chain at all — which is three days of the exact exposure it
was installed to prevent. The shortcut is a UTXO snapshot of about 8.7 GB,
fetched from this project's release page, which brings that down to under an
hour.

It is offered, never assumed. Nothing is downloaded until you press the button;
an installation whose owner never does makes no outbound request at all. The
`auto_start` setting exists for deployments with nobody at a screen, and it is
off unless somebody sets it.

### Why you are not trusting whoever hosts it

The snapshot's base block hash is compiled into **Bitcoin Core**. When the file
is loaded, Core recomputes the hash of the UTXO set it has just read and compares
it against its own built-in value. A corrupted download, a botched reassembly and
a deliberately altered file all produce the same outcome: the node refuses it.
Whoever serves the file cannot make your node accept a state Core does not
already agree with.

The per-part checksums Forktower carries are a convenience on top of that — they
catch a bad part after two gigabytes instead of after nine. They are compiled
into the binary rather than downloaded, because a checksum fetched from the same
host as the file it vouches for is not a check. The same is true of the mirror
setting: it changes *where* the parts are fetched from and cannot change what
they must contain.

### What it costs you, stated plainly

The request goes through the same Tor proxy the second node uses for its peering,
and the hostname is sent to the proxy unresolved — so no DNS query for the
download host leaves your network either. That is not decoration. A direct
request for this specific file, from a residential address, says that whoever
lives there runs Lightning channels and is preparing to defend them across a
split, which is targeting information.

If you have turned clearnet peering on for the second node, the download follows
that decision and goes direct, and the daemon says so in its log on every start.

Everything below the snapshot's height is still validated in full, in the
background, after the shortcut — the node is not being asked to skip verification,
only to defer it.

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
a file, or touching the database. They take facts and return verdicts. This is
enforced by the linter, per file, with a reason attached to each rule — so the
question "could this have been influenced by something other than its inputs?" is
answerable by reading one screen.

Two of them — whether a transaction may be copied, and whether a channel is
covered — additionally cannot read a clock, so the same question always gets the
same answer. The other two use clock *types* to express durations and take the
current time as an argument rather than fetching it, which is the same property
arrived at by a different route; the linter cannot express that distinction, so
it is stated here rather than claimed as enforced.

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
