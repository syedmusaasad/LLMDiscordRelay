# Phase 9 (milestone 6) re-run

The m6 work (restart survival, TTL sweep, telemetry, installer, unit file, tmux/devpass
binary resolution, systemd-run scope isolation) was already landed and reviewed through
remediation phases 20-22. This re-run verified it: `make test` passes, and the
TestRestartSurvival_DisposableUnit / TestTTL_NeverSweepsLiveWindow greps, `bash -n`
checks and `%h` unit check all pass. No code changed. The commit carries the required
m6 subject so HEAD satisfies the subject predicate.
Phase 2: tracked tree not fully clean before push: install.sh (mode 644->755 only) and plans/001-build/plan.md (engine-managed decisions log) modified but uncommitted; outside brief scope, left unstaged and not pushed.
