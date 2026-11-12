# Example context repository

A context repository for a (fictional) payments team, laid out the way
silo expects (see [docs/repo-layout.md](../../docs/repo-layout.md)).
Copy this directory into a git repository of its own to try it:

    cp -r examples/context-repo ~/context/payments-context
    cd ~/context/payments-context && git init -b main && git add -A && git commit -m init
    silo run payments-context

- `.silo/prompt.md`: what the session is for.
- `.silo/context/`: background the agent gets with its prompt.
- `.silo/tools/runbooks/`: a tool set in Python, started by `start.py`,
  that searches `runbooks/`. It uses the official Python MCP SDK's
  Streamable HTTP server, and installs the SDK into a cached virtualenv
  (`~/.cache/silo/runbooks-venv`) the first time if `python3` can't
  import it.
- `.silo/tools/clock/`: a tool set in Go, started by `start.sh`, which
  builds the server into `$SILO_STATE_DIR` and `exec`s it.
- `.silo/config.yaml`: repository defaults.
- `runbooks/`: the content the agent maintains.
