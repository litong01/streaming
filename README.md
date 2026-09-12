# Streaming

Tablet-based control of Extron SMP 300 Series streaming presets. One Go control
server is used everywhere: as a standalone executable on computers and as an
ARM64 executable supervised by the Android app.

## Android behavior

- Tapping the Streaming app icon starts the foreground web server and opens its
  configuration page inside the app.
- The APK contains the Go control server and a go2rtc binary. A small Kotlin
  foreground service starts the Go server, which in turn starts go2rtc for the
  live preview.
- The configuration page collects the SMP address, SSH credentials, server
  port, preset numbers, and the SMP confidence/secondary stream URL used for
  the on-screen preview.
- Credentials never leave the Android device.
- Fully Kiosk Browser uses `http://127.0.0.1:8080/` for daily operation.
- The server starts during boot, before anyone signs in, and again after an app
  update. Every fifteen minutes a background check restarts it if it stopped
  answering.
- Preset 1 is Mandarin and preset 2 is English by default. Both presets must
  already be configured through the native SMP web interface.

The Android wrapper is Kotlin, but SMP commands, status polling, configuration
APIs, and web serving are implemented only in Go. No Java or Android tooling is
required on your Mac because GitHub Actions performs the Android build.

## Web pages

The Go server embeds the two pages from `web/`. The control page follows the
myconsole launcher look used by Fully Kiosk Browser: clock, date, a small live
preview above the round tiles, and English / Mandarin / Stop. The preview is
shown only while a stream is enabled. go2rtc converts the SMP secondary output
into a browser-playable stream so the picture matches what is pushed to YouTube.

## Build and download the APK

GitHub Actions builds the APK and publishes it as a GitHub Release. No Java or
Android SDK is needed on your Mac.

1. Push this repository to GitHub, or run **Actions → Build Android APK**.
2. When the workflow succeeds, open **Releases**.
3. Download `streaming-debug.apk` from the latest release.
4. Install that APK on the tablet.

Each successful `main` build or manual workflow run creates a new release tagged
`apk-<run number>`. Pull requests only upload a workflow artifact; they do not
create a release.

## Using the app

1. Install and tap the Streaming icon.
2. Grant notification permission so Android can show the server's persistent
   foreground-service notification.
3. Enter the SMP address (`host` or `host:22023`), username, password,
   and the local server port (default `8080`). Paste the SMP confidence /
   secondary RTSP URL from the SMP Device Status page (for example
   `rtsp://192.168.1.10/extron2`). The SSH port is `22023` unless you include
   a different one after the colon.
4. Save the configuration.
5. Point Fully Kiosk Browser at `http://127.0.0.1:8080/`.

If Fully Kiosk runs on another tablet, use
`http://<server-tablet-ip>:8080/` instead.

## Keeping the server running unattended

The app restarts itself during boot, but Android will only let it do so if the
tablet is set up for unattended use.

- **A screen lock is fine.** The server is direct boot aware: it keeps its
  settings in device-protected storage, which Android decrypts at power-on
  without a PIN, so the control page answers while the tablet still sits at its
  lock screen.
- **Allow autostart.** Many tablets gate boot broadcasts behind a vendor
  setting, usually App info → Autostart or a bundled Security app. Android
  delivers nothing to the app until it is switched on.
- **Accept the battery prompt** shown the first time the app opens. That dialog
  is offered once per install, so afterwards turn on **Allow background usage**
  under App info → Battery for Streaming instead.
- **Allow notifications.** The server runs as a foreground service, and Android
  stops the service if its notification is blocked.
- **Never use Force stop.** Android then withholds boot broadcasts from the app
  until someone opens it by hand.

To confirm the tablet behaves after a power cut, reboot it, leave it at the
lock screen for a minute, and check that `http://127.0.0.1:8080/api/status`
answers from another machine on the network. Over adb,
`adb logcat -s StreamingBootReceiver StreamingForegroundSvc StreamingWatchdog`
shows which trigger started the server.

Because the settings sit in device-protected storage, they are guarded by the
app sandbox and the tablet's hardware key rather than by the screen lock. That
is the trade for a server that runs without anyone signing in. Reading them
needs root or an unlocked bootloader.

## Standalone Go server

The same implementation packaged in the APK can run directly on a Mac, Linux
computer, Raspberry Pi, or Termux:

```bash
go build -o streaming .
./streaming
```

Its configuration is stored at
`~/.config/streaming/config.json` with file mode `0600`. Override the path with
the `STREAMING_CONFIG` environment variable.

On a computer, install [go2rtc](https://github.com/AlexxIT/go2rtc/releases) on
your `PATH`, place the binary next to `./streaming`, or set `STREAMING_GO2RTC`
to its path. The control server writes `go2rtc.yaml` and starts that process on
port `1984` (configurable).

## Android build details

The Gradle build invokes `scripts/build-android-go.sh`, which uses the Android
NDK to compile ARM64 PIE executables for the control server and go2rtc. They
are packaged as `lib/arm64-v8a/libstreaming.so` and `lib/arm64-v8a/libgo2rtc.so`
so Android extracts them into the app's executable native-library directory.

On upgrade from the earlier Kotlin-server APK, existing encrypted SMP settings
are imported once. The Go configuration file is encrypted with an AES-GCM key,
and both live in device-protected storage so the service can read them during
boot. Settings written by an older build sit in credential-protected storage
and are copied across the first time the updated app runs with the tablet
unlocked, which is the moment the APK is installed.

## SMP commands

For stream 1 (Archive Ch A) and preset `P`:

- Recall streaming preset: `3*1*P.`
- Enable stream: `E1*1STRC}`
- Disable stream: `E1*0STRC}`
- Query stream enabled: `E1STRC}`
- Query selected streaming preset: `46I`

In Extron's command-table notation, `E` is the escape byte (`0x1b`), `}` is
a carriage return (`0x0d`), and `]` in a response is CR/LF. They are not
literal characters. The Go client sends and reads those control bytes.

The app always controls Archive Channel A. That is the encoder used for the
YouTube live push. The on-screen player uses the SMP confidence/secondary
output through go2rtc, so the tablet shows the same encoded picture.
