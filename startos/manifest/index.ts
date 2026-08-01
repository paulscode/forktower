import { setupManifest } from '@start9labs/start-sdk'
import { appVersion } from '../version'

const short = 'Watches the other chain, so a split does not cost you a channel'

const long =
  'During a contested Bitcoin upgrade the network can separate into two chains. ' +
  'Your node follows one of them and is blind to the other — but your Lightning ' +
  'channels exist on both, and the timers that protect them keep running on both. ' +
  'Forktower runs a second Bitcoin node following the chain yours does not, tells ' +
  'you which of your channels would be exposed, and how long you have. It can also ' +
  'watch a watchtower on the other chain and copy your own closing transactions ' +
  'across. It only ever reads from your Lightning node: it holds no keys, signs ' +
  'nothing, and cannot move your money.'

export const manifest = setupManifest({
  id: 'forktower',
  title: 'Forktower',
  license: 'MIT',
  packageRepo: 'https://github.com/paulscode/forktower',
  upstreamRepo: 'https://github.com/paulscode/forktower',
  marketingUrl: 'https://github.com/paulscode/forktower',
  donationUrl: null,
  description: { short, long },
  // One data volume: the daemon's database and configuration, plus the second
  // Bitcoin node's datadir under `sq/`. Dependency volumes are mounted in
  // main.ts and are deliberately not declared here — see the mount comment
  // there, which is about the wallet seed rather than about plumbing.
  volumes: ['main'],
  images: {
    main: {
      source: {
        dockerBuild: {
          dockerfile: 'Dockerfile',
          workdir: '.',
          // Without this the daemon inside the package reports itself as
          // `dev`, which is the version a user would quote in a bug report.
          buildArgs: { VERSION: appVersion },
        },
      },
      arch: ['x86_64', 'aarch64'],
    },
  },
  // **Every `metadata.icon` here must actually resolve.** The packer fetches
  // them at pack time, and a 404 fails the build with
  // `cannot filter out unhashed file icon.ico, run update_hashes first` — an
  // error that says nothing about the URL it could not fetch. Two of these were
  // wrong on the first attempt and cost an afternoon. Check them with curl
  // before changing one; the Core Lightning package's id is `c-lightning` but
  // its repository is `cln-startos`, which is exactly the sort of thing that
  // makes a guessed URL look plausible.
  dependencies: {
    // Required. Forktower's whole subject is the difference between two chains,
    // and one of the two is the chain the user's own node follows. Without it
    // there is no comparison to make and nothing worth reporting.
    bitcoind: {
      description:
        'The Bitcoin node you already run. Forktower reads its chain and ' +
        'compares it against the other one.',
      optional: false,
      metadata: {
        title: 'Bitcoin Core',
        icon: 'https://raw.githubusercontent.com/Start9Labs/bitcoind-startos/master/icon.svg',
      },
    },
    // Optional, and genuinely so. Forktower is useful with no Lightning node at
    // all — watching the two chains is what it does first. A node is what lets
    // it say *which of your channels* is exposed rather than only that a split
    // happened.
    lnd: {
      description:
        'Your Lightning node, if you run LND. Forktower reads your channels ' +
        'from it — never writes — to tell you which ones a split would expose.',
      optional: true,
      metadata: {
        title: 'LND',
        icon: 'https://raw.githubusercontent.com/Start9Labs/lnd-startos/master/icon.svg',
      },
    },
    'c-lightning': {
      description:
        'Your Lightning node, if you run Core Lightning. Read-only, for the ' +
        'same reason and to the same end as LND.',
      optional: true,
      metadata: {
        title: 'Core Lightning',
        icon: 'https://raw.githubusercontent.com/Start9Labs/cln-startos/master/icon.svg',
      },
    },
  },
  alerts: {
    install:
      'Forktower runs a second Bitcoin node, following the chain your own node ' +
      'is not. Expect it to use disk and bandwidth of its own, and to take a ' +
      'while to catch up the first time — pruned by default, which is enough ' +
      'for everything it does. It reads from your Lightning node and never ' +
      'writes: it holds no keys and cannot move your money. It cannot close a ' +
      'channel for you either. What it can do is tell you, in time, that you ' +
      'should.',
  },
})
