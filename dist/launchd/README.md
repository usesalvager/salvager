# Auto-start the watcher (macOS, launchd)

Runs `salvager watch` for this project automatically: starts at login, restarts
if it dies, survives reboot.

The plist holds absolute paths — edit them for your machine before installing:

- `ProgramArguments[0]` → path to the `salvager` binary (`go install .` puts it in
  `$(go env GOPATH)/bin/salvager`)
- `WorkingDirectory` → the project root to watch
- `Standard{Out,Error}Path` → log destinations

## Install

```sh
cp com.salvager.watch.plist ~/Library/LaunchAgents/
launchctl load -w ~/Library/LaunchAgents/com.salvager.watch.plist
launchctl list | grep salvager    # confirm it's running
```

## Uninstall / restart

```sh
launchctl unload ~/Library/LaunchAgents/com.salvager.watch.plist   # stop
launchctl load   -w ~/Library/LaunchAgents/com.salvager.watch.plist # start
```

To watch a second project, copy the plist under a new `Label` and adjust
`WorkingDirectory`.
