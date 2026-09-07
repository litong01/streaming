# Streaming

Tablet-based control of Extron SMP 300 Series streaming presets. One Go control
server is used everywhere: as a standalone executable on computers and as an
ARM64 executable supervised by the Android app.

## Android behavior

- Tapping the Streaming app icon starts the foreground web server and opens its
  configuration page inside the app.
- The APK contains the Go control server. A small Kotlin foreground service
  starts it, restarts it after an unexpected exit, and keeps Android from
  suspending it.
- The configuration page collects the SMP address, SSH credentials, server
  port, and preset numbers.
- Credentials are encrypted and stored only on the Android device.
- Fully Kiosk Browser uses `http://127.0.0.1:8080/` for daily operation.
- The server starts again after tablet reboot.
- Preset 1 is Mandarin and preset 2 is English by default. Both presets must
  already be configured through the native SMP web interface.

The Android wrapper is Kotlin, but SMP commands, status polling, configuration
APIs, and web serving are implemented only in Go. No Java or Android tooling is
required on your Mac because GitHub Actions performs the Android build.

## Web pages

The Go server embeds the two pages from `web/`. The control page follows the
myconsole launcher look used by Fully Kiosk Browser: clock, date, and round
tiles for English, Mandarin, and Stop. The same embedded pages are served by
the desktop executable and by the executable packaged in the APK.

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
   and the local server port (default `8080`). The SSH port is `22023`
   unless you include a different one after the colon.
4. Save the configuration.
5. Point Fully Kiosk Browser at `http://127.0.0.1:8080/`.

If Fully Kiosk runs on another tablet, use
`http://<server-tablet-ip>:8080/` instead.

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

## Android build details

The Gradle build invokes `scripts/build-android-go.sh`, which uses the Android
NDK to compile an ARM64 PIE executable. It is packaged as
`lib/arm64-v8a/libstreaming.so` so Android extracts it into the app's executable
native-library directory. The Kotlin foreground service launches that file.

On upgrade from the earlier Kotlin-server APK, existing encrypted SMP settings
are imported once. The Go configuration file is encrypted with an AES-GCM key
kept in Android Keystore-backed preferences.

## SMP commands

For stream 1 (Archive Ch A) and preset `P`:

- Recall streaming preset: `3*1*P.`
- Enable stream: `E 1*1 STRC}`
- Disable stream: `E 1*0 STRC}`
- Query stream enabled: `E 1)STRC}`
- Query selected streaming preset: `46I`

The app always controls Archive Channel A. That is the encoder used for the
YouTube live push.
