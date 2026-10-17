# team/: the Nervus-OS team repositories

Everything the Nervus-OS team keeps on GitHub (github.com/Nervus-OS) lives in this repository as folders, so
NervusOS-Cambridge is the one place the whole project can be read. Chinese text has been translated to English.

`src/` holds the code this machine actually builds and runs. It was forked from the team repositories on
2026-08-11 (the exact commits are in `src/UPSTREAM.lock`) and has moved on since: hosted mode, the PiPER provider,
the ACT system. `team/` holds what is in the team's own repositories now, wherever that differs from `src/`.

| Folder | Repository | Imported at | What it is |
|---|---|---|---|
| `nervud/` | Nervus-OS/nervud | `195f3df`, 2026-10-04 | The team's restart of nervud: a small new kernel (module registry, Unix-socket server, virtual service). It replaced the 57,000-line daemon that `src/nervud` is built from. |
| `nervus-ipc/` | Nervus-OS/nervus-ipc | `e9356e6`, 2026-09-30 | The protocol restarted alongside it: a few protobuf files and their Go bindings. `src/nervus-ipc` is the full protocol the running system uses. |

The other team repositories (nervus-app-sdk, nervus-app-example, nervus-packaging, nervus-system-server,
nervus-system-software, nervus-deploy, nring, robot-arm) have not changed since the fork: their upstream heads
are the commits in `src/UPSTREAM.lock`, so `src/` is their copy.
