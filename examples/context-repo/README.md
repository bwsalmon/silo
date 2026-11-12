# Example context repository

A context repository for a (fictional) payments team, laid out the way
silo expects (see [docs/repo-layout.md](../../docs/repo-layout.md)).
Copy this directory into a git repository of its own to try it:

    cp -r examples/context-repo ~/context/payments-context
    cd ~/context/payments-context && git init -b main && git add -A && git commit -m init
    silo run payments-context

- `.silo/prompt.md`: what the session is for.
- `.silo/context/`: background the agent gets with its prompt.
- `.silo/tools/runbooks/`: a tool set written in Python (standard library
  only) that searches `runbooks/`.
- `.silo/tools/clock/`: a tool set written in Go, built when the session
  starts.
- `.silo/config.yaml`: repository defaults.
- `runbooks/`: the content the agent maintains.
