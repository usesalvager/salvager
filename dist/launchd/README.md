# Auto-start the watcher (macOS, launchd)

Runs `lochis watch` for this project automatically: starts at login, restarts
if it dies, survives reboot.

The plist holds absolute paths — edit them for your machine before installing:

- `ProgramArguments[0]` → path to the `lochis` binary (`go install .` puts it in
  `$(go env GOPATH)/bin/lochis`)
- `WorkingDirectory` → the project root to watch
- `Standard{Out,Error}Path` → log destinations

## Install

```sh
cp com.lochis.watch.lochis-ai.plist ~/Library/LaunchAgents/
launchctl load -w ~/Library/LaunchAgents/com.lochis.watch.lochis-ai.plist
launchctl list | grep lochis    # confirm it's running
```

## Uninstall / restart

```sh
launchctl unload ~/Library/LaunchAgents/com.lochis.watch.lochis-ai.plist   # stop
launchctl load   -w ~/Library/LaunchAgents/com.lochis.watch.lochis-ai.plist # start
```

To watch a second project, copy the plist under a new `Label` and adjust
`WorkingDirectory`.
