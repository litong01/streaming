# Streaming

Tablet-based control of Extron SMP 300 Series streaming presets. The repository
contains an Android app for the tablet and an optional standalone Go server.

## Android behavior

- Tapping the Streaming app icon starts the foreground web server and opens its
  configuration page inside the app.
- The configuration page collects the SMP address, SSH credentials, server
  port, stream index, and preset numbers.
- Credentials are encrypted and stored only on the Android device.
- Fully Kiosk Browser uses `http://127.0.0.1:8080/` for daily operation.
- The server starts again after tablet reboot.
- Preset 1 is English and preset 2 is Mandarin by default. Both presets must
  already be configured through the native SMP web interface.

The Android source is Kotlin, but no Java or Android tooling is required on
your Mac. GitHub Actions performs the Android build.

## Build the APK with GitHub Actions

1. Push this repository to GitHub.
2. Open the repository's **Actions** tab.
3. Select **Build Android APK**.
4. Choose **Run workflow**.
5. When it finishes, open the run and download the
   `streaming-debug-apk` artifact.
6. Extract and install `app-debug.apk` on the tablet.

The workflow also runs automatically when Android files are pushed to `main`.
Its JDK and Android SDK exist only on the GitHub runner.

The debug APK is installable directly. A future Play Store release will need a
release signing key stored as GitHub Actions secrets.

## Using the app

1. Install and tap the Streaming icon.
2. Grant notification permission so Android can show the server's persistent
   foreground-service notification.
3. Enter the SMP hostname/IP, SSH port (default `22023`), username, password,
   and the local server port (default `8080`).
4. Save the configuration.
5. Point Fully Kiosk Browser at `http://127.0.0.1:8080/`.

If Fully Kiosk runs on another tablet, use
`http://<server-tablet-ip>:8080/` instead.

## Optional standalone Go server

The Go implementation offers the same web controls for a Mac, Linux computer,
Raspberry Pi, or Termux:

```bash
go build -o streaming .
./streaming
```

Its configuration is stored at
`~/.config/streaming/config.json` with file mode `0600`. Override the path with
the `STREAMING_CONFIG` environment variable.

## SMP commands

For stream index `N` and preset `P`:

- Recall streaming preset: `3*N*P.`
- Enable stream: `E N*1 STRC}`
- Disable stream: `E N*0 STRC}`
- Query stream enabled: `E N)STRC}`
- Query selected streaming preset: `46I`, `47I`, or `48I`

The default stream index is `1` (Archive Ch A).
