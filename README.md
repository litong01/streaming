# Streaming

Tablet-based control of Extron SMP 300 Series streaming. One Go control server
is used everywhere: as a standalone executable on computers and as an ARM64
executable supervised by the Android app.

Start Mandarin and Start English press the unit's own **START RTMP STREAM**
buttons, over the same web API its configuration page uses. Nothing here
recalls a preset or edits a destination.

## Android behavior

- Tapping the Streaming app icon starts the foreground web server and opens its
  configuration page inside the app.
- The APK contains the Go control server. A small Kotlin foreground service
  starts it.
- The configuration page collects the SMP address, its credentials, and the
  server port. There is nothing else to set.
- Credentials never leave the Android device.
- Fully Kiosk Browser uses `http://127.0.0.1:8080/` for daily operation.
- The server starts during boot, before anyone signs in, and again after an app
  update. Every fifteen minutes a background check restarts it if it stopped
  answering.
- The Archive encoder carries Mandarin and the Confidence encoder carries
  English. Both destinations are configured on the SMP itself, through its own
  web interface, and this app never changes them.

The Android wrapper is Kotlin, but SMP commands, status polling, configuration
APIs, and web serving are implemented only in Go. No Java or Android tooling is
required on your Mac because GitHub Actions performs the Android build.

## Web pages

The Go server embeds the two pages from `web/`. The control page follows the
myconsole launcher look used by Fully Kiosk Browser: clock, date, a small live
preview above the round tiles, and Start English / Start Mandarin / Stop.

The preview is the SMP's own `/mp4stream` endpoint, a fragmented MP4 the unit
serves over HTTP and plays in its own web interface. `/api/preview` relays it
with the stored credentials so the browser needs none. The unit produces it
whether or not anything is streaming, so the camera is visible before anyone
taps a language. This replaced an RTSP pull through go2rtc, which this unit
accepts and then never sends media on: it reports 0 packets sent for every
session, over both TCP and UDP.

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
3. Enter the SMP address (`host` or `host:443`), username, password, and the
   local server port (default `8080`). These are the same credentials as the
   SMP's own web page. The port is `443` unless you include a different one
   after the colon; `80` switches to plain HTTP.
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

## Android build details

The Gradle build invokes `scripts/build-android-go.sh`, which uses the Android
NDK to compile an ARM64 PIE executable for the control server. It is packaged
as `lib/arm64-v8a/libstreaming.so` so Android extracts it into the app's
executable native-library directory.

On upgrade from the earlier Kotlin-server APK, existing encrypted SMP settings
are imported once. The Go configuration file is encrypted with an AES-GCM key,
and both live in device-protected storage so the service can read them during
boot. Settings written by an older build sit in credential-protected storage
and are copied across the first time the updated app runs with the tablet
unlocked, which is the moment the APK is installed.

## SMP commands

Everything goes through `/api/swis/resources` on the SMP's web port, the same
API its own configuration page uses. A `GET` takes one `uri` parameter per
resource wanted, and a `PUT` takes a list of resources and values:

```
GET /api/swis/resources?uri=/streamer/rtmp/1/pub_control
PUT /api/swis/resources   [{"uri": "/streamer/rtmp/1/pub_control", "value": 1}]
```

Archive is channel 1 and Confidence is channel 3. Four resources matter:

- `/streamer/rtmp/N/pub_control` is the RTMP push itself, and what the unit's
  START and STOP RTMP STREAM buttons write.
- `/encoder/N/stream_enable` is the encoder behind it.
- `/streamer/rtmp/N` reports `session_info`, whose `resolved_ip` names the
  address a live push has actually connected to.
- `/mp4stream` is the live preview.

Authentication is HTTP Basic with the unit's own web credentials. The
certificate is self-signed by Extron, so it is not verified; the alternative is
plain HTTP to the same unit over the same wire.

Two details of the unit's behavior shape the code:

**A publish is refused unless the encoder is already running.** `pub_control =
1` on a stopped encoder answers `E13 Invalid value`, so a start writes
`stream_enable = 1` first. That write is harmless when the encoder is already
running, which it normally is.

**Errors arrive as HTTP 200.** A refused command comes back with an Extron code
in `meta.status` beside the resource it refused, so the body is what says
whether a call worked, not the status code.

Starting a language reads both channels before writing anything. The other
language is stopped only if it is actually publishing, and a language that is
already live is left completely alone rather than being interrupted and
restarted. The start is then confirmed by waiting for `resolved_ip` to name a
real address: the publish flag is set the moment the command is accepted, which
is before the destination has been contacted, so a push that is going to be
refused would otherwise be announced as a live stream.

Stop clears `pub_control` on whichever channel is publishing and leaves the
encoders themselves running. Switching an encoder off is what makes the unit
report its configuration as `modified, not saved`, which its own page renders
as a blank selection with half the streaming section greyed out.

## Checking the connection

**Test connection** on the configuration page checks both things the control
page needs, the API that starts a stream and the preview that shows the
picture, and names the layer that failed: the address, the network, the
credentials, the API, or the preview. A failure on the preview alone still
leaves the buttons working.

`scripts/smp-api.sh` does the same from a computer, and can start and stop the
streams too:

```bash
SMP_HOST=192.168.1.10 SMP_PASSWORD_FILE=~/.smp-password ./scripts/smp-api.sh status
```

`TestLiveSMP` prints what the real unit reports. It is skipped unless
`SMP_HOST` is set, and every call it makes is a read, so it is safe to run
during a service:

```bash
SMP_HOST=192.168.1.10 SMP_USER=admin SMP_PASSWORD_FILE=~/.smp-password \
  go test -run TestLiveSMP ./internal/smp/ -v
```

A status poll is skipped rather than queued while a button press or a
connection test is in flight, so what the page shows is never a reading from
the middle of a language switch.
