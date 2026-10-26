// Package e2e holds silo v3's milestone-1 end-to-end test (build tag e2e).
//
// It brings up the whole stack (the gateway, the session bridge and web UI,
// and both standing agents, root and the default user agent, connected with
// silo connect) and drives it through the UI's HTTP API the way the human
// would: chat with the user agent, approve a run_as_root batch the root
// agent proposes and find it in the hash-chained audit log, and approve a
// child agent the user agent registers and starts with silo connect, chat
// with it and stop it.
//
// It must run as root, because the gateway does:
//
//	cd v3 && sudo -E go test -tags e2e -v ./e2e
//
// By default it starts the stack from a fresh build in an isolated
// directory, as plain processes, and stops it afterwards. With
// SILO_E2E_INSTALL=1 it instead runs deploy/install.sh on this host (a
// Debian 12/13 VM with systemd) and tests the installed units;
// SILO_E2E_UNINSTALL=1 then also runs deploy/uninstall.sh --purge at the
// end.
//
// The agents run fakeagent (./fakeagent), a scripted ACP CLI, so no model
// credentials are needed. SILO_E2E_REAL_CLI=gemini runs the standing agents
// on the real Gemini CLI instead (it must be installed, and GEMINI_API_KEY
// set); the test then asks the model in plain words to make the same tool
// calls, which is best-effort by nature.
package e2e
