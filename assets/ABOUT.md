# About Forktower

Forktower watches the Bitcoin chain your own node is not following.

During a contested upgrade the network can separate into two chains. Your node
follows one; your Lightning channels exist on both, and so do the timers that
protect them. A node cannot respond to a breach on a chain it cannot see, and
cannot tell you it is happening.

Forktower runs a second Bitcoin node on the other chain and answers what yours
cannot: whether the chains have really separated, which of your channels are
exposed, and how long you have.

It only ever reads from your Lightning node. It holds no keys and signs nothing.

Source: https://github.com/paulscode/forktower
