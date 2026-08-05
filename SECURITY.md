# Reporting a security problem

**Please do not open a public issue for anything that could be used to lose
somebody money.**

Email **paul@paulscode.com**. If you would rather encrypt it, the release signing
key published with each release will reach me.

Tell me what you found, how to reproduce it if you can, and what you think it
lets an attacker do. A rough description of something you are not sure about is
worth more than silence — I would much rather read ten reports that turn out to
be nothing than miss the one that was not.

I will acknowledge within a few days. If a fix is needed I will tell you what it
is and when it lands, and you will be credited in the release notes unless you
would rather not be.

## What is worth reporting

Anything that could cause Forktower to:

- **say a channel is safe when it is not**, or fail to say one is at risk;
- **report the two chains as in step when they have separated**, or the reverse;
- **miss a breach or a close** it should have detected;
- **leak a credential**, a macaroon, a rune, or anything else that could move
  money;
- **be made to act on instructions from somebody other than its operator**.

The first two matter most. Forktower's entire job is to tell you the truth about
something you cannot see for yourself, so a bug that makes it lie confidently is
worse than one that makes it crash. A crash is visible.

## What is already known

Some limits are inherent rather than bugs, and they are written down in
[Residual risks](docs/residual-risks.md). If you find something in that list, it
is not a vulnerability — though if you think one of them is worse than the
document admits, that *is* worth telling me.

## Scope

The daemon, the packaging for StartOS and Umbrel, the dashboard, and the release
process. Forktower reads your Bitcoin and Lightning nodes and never writes to
them, so a problem in this software should not be able to move your money — if
you find a path where it could, that is the most serious kind of report and I
would like to hear it immediately.

Out of scope: Bitcoin Core, LND, Core Lightning, StartOS and Umbrel themselves.
Report those upstream. If a problem in one of them is made worse by how Forktower
uses it, that part is mine.
