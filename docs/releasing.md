# Cutting a release

For whoever maintains Forktower. Users do not need this.

## The short version

```
make release          # builds everything, then stops
                      # ... sign SHA256SUMS on the airgapped machine ...
make verify-release   # refuses if it is not signed
```

Nothing is published until `verify-release` passes.

## Why it stops

`make release` deliberately cannot finish the job. The signing key lives on an
airgapped machine and is never on a build host, so there is no `make sign` and
there should not be one. What the build does is put everything in one directory
with a `SHA256SUMS` beside it, and then print exactly what has to happen next.

## Step by step

### 1. Set the version

It is written in two places and both are checked:

- `manifest.yaml` — the source of truth, used by the 0.3.5.1 package
- `startos/version.ts` — `appVersion`, used by the 0.4.x package

`make check-versions` fails if they disagree, and also if the checkout is on a
git tag that does not match. A release where these differ would install claiming
one version and run reporting another, and the number a user quotes in a bug
report would be the wrong one.

### 2. Build

```
make release
```

This runs `make check` first — a release cut from a tree that does not pass its
own gate is exactly what step 4 exists to catch, and catching it before a
twenty-minute build is cheaper than after. Then it builds both packages, writes
`SBOM.md`, and checksums everything into `builds/<version>/`:

```
forktower-0351.s9pk     StartOS 0.3.5.x, both architectures
forktower-040.s9pk      StartOS 0.4.x, both architectures
SBOM.md                 what is inside this release
SHA256SUMS              checksums of all of the above
```

### 3. Sign, on the airgapped machine

Copy `SHA256SUMS` across, then:

```
gpg --detach-sign --armor SHA256SUMS
```

Copy `SHA256SUMS.asc` back into `builds/<version>/`. The private key does not
travel and neither does anything else.

### 4. Verify, here

```
make verify-release
```

It checks the signature with the **public** key — the same way a user would —
and then that every file matches its checksum. Checking with the private key
would prove that the machine holding it can verify its own work, which is not
the question anyone is asking.

**This step exists because of a specific mistake.** A sibling project shipped one
version unsigned while every release before and after it was signed. Nobody
noticed, because nothing was checking: a directory with no `.asc` in it looks
exactly like a directory you have seen a hundred times. So the build refuses
rather than relying on anyone to look.

### 5. Publish

- **StartOS**: upload the two `.s9pk` files and `SHA256SUMS.asc` to the release.
- **Umbrel**:

  ```
  make image-push          # both architectures, to the registry
  make umbrel-pin          # rewrite the compose to pin the exact image
  make umbrel-sync         # copy into the store repository checkout
  ```

  then commit and push the store repository.

  `umbrel-pin` exists because the digest to pin is not the obvious one. A
  multi-architecture tag is an *index* listing one image per platform, and
  `docker manifest inspect` shows those per-platform digests prominently — they
  are the easiest thing to copy and the wrong thing to pin. Pinning one would
  serve that architecture and leave everybody else unable to install, which for
  this project means the Raspberry Pi users. The target reads the index digest,
  and refuses a tag that is not pushed, one that is a single image, or one
  missing an architecture somebody can currently install on.

On that last point: while an app is in testing its compose file points at a
moving `:dev` tag, which is convenient for us and wrong for a user. A published
release must pin `image: paulscode/forktower:<version>@sha256:...` so that
whoever installs it gets the bits that were tested rather than whatever the tag
happens to point at that day.

## Publishing a UTXO snapshot

Separate from a software release, and on a different schedule: the snapshot's
height is fixed by Bitcoin Core's own hardcoded assumeutxo values, so it moves
when Core does and not when we do. It lives on its own tag —
`utxo-snapshot-935000` — published as a pre-release so it does not compete for
"Latest" with an actual version.

The parts and their checksums are compiled into the daemon, in
`internal/bootstrap/snapshot.go`, and every one of those values was transcribed
by hand. **A digit dropped from a part length is not a compile error and no
offline test can see it** — it is a download that runs for hours and fails at the
end, on somebody else's machine. So after publishing, ask the release:

```
FORKTOWER_LIVE=1      go test -run TestLive ./internal/bootstrap/
FORKTOWER_LIVE_FULL=1 go test -run TestLive ./internal/bootstrap/   # ~2 GB
```

The first checks every part's name, size and URL against what GitHub actually
serves, and runs a small real transfer through the redirect and range handling.
The second fetches a whole part and checks it against its compiled-in digest.
Neither runs under `make check`, deliberately: a gate that depends on a network
turns a GitHub outage into a failing build, and a build that fails for reasons
unrelated to the code stops meaning anything.

## What is deliberately not automated

**Signing.** Covered above.

**Publishing.** Both stores are a deliberate human action. There is no step here
that pushes anything anywhere without somebody typing it.

**Anything that talks to the network during a build**, beyond fetching the pinned
third-party binaries — each of which is checked against a checksum committed in
this repository, never one fetched alongside the download. A checksum an attacker
can replace along with the file is not a check.
