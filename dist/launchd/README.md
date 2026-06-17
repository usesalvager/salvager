# Auto-start the watcher (macOS, launchd)

The supported way to run the watcher persistently is the built-in command — it
generates a per-project LaunchAgent (absolute binary + absolute `--root`, a
unique per-project label, per-project logs), loads it with the modern
`launchctl bootstrap`, and **verifies it is actually running** before reporting
success:

```sh
salvager service install     # start now + on every login/reboot
salvager service status       # installed? running? persistent? (--json for scripts)
salvager service uninstall    # stop and remove cleanly
```

Prefer that over hand-editing a plist. The `com.salvager.watch.plist` here is a
reference template only.

## Manual install (reference)

If you want to wire it by hand, edit the absolute paths in the plist first
(`ProgramArguments` → the `salvager` binary and `watch --root <project>`,
`Standard{Out,Error}Path` → log destinations), give it a unique `Label` per
project, then:

```sh
cp com.salvager.watch.plist ~/Library/LaunchAgents/com.salvager.watch.myproject.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.salvager.watch.myproject.plist
launchctl print gui/$(id -u)/com.salvager.watch.myproject | grep state   # expect: state = running
```

Stop and remove:

```sh
launchctl bootout gui/$(id -u)/com.salvager.watch.myproject
rm ~/Library/LaunchAgents/com.salvager.watch.myproject.plist
```

`RunAtLoad=true` + `KeepAlive=true` make it start immediately and restart on
death/login/reboot. Use a distinct `Label` (and log paths) per project — the
built-in `salvager service install` derives both from a hash of the project root
automatically, which is why it's the recommended path.
